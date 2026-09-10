using System.Collections.Concurrent;
using System.Diagnostics;
using System.Globalization;
using System.Security.Cryptography;
using System.Text.Json;
using System.Threading.Channels;
using Npgsql;
using NpgsqlTypes;

var exitCode = await BomberLoadTestRunner.RunAsync(args);
Environment.ExitCode = exitCode;

internal static class BomberLoadTestRunner
{
    private static readonly Guid DefaultUserTaskTypeId = Guid.Parse("00000000-0000-0000-0000-0000000000aa");
    private const int ReconnectBaseDelayMs = 10;
    private const int ReconnectMaxDelayMs = 1000;

    public static async Task<int> RunAsync(string[] args)
    {
        RunnerOptions options;
        try
        {
            options = RunnerOptions.Parse(args);
        }
        catch (Exception ex)
        {
            Console.Error.WriteLine($"Argument error: {ex.Message}");
            RunnerOptions.PrintUsage();
            return 2;
        }

        if (options.ShowHelp)
        {
            RunnerOptions.PrintUsage();
            return 0;
        }

        var roMixPath = Path.Combine(options.LoadtestDir, "config", "ro_mix.tsv");
        var rwMixPath = Path.Combine(options.LoadtestDir, "config", "rw_mix.tsv");
        var snippetDir = Path.Combine(options.LoadtestDir, "sql", "snippets");

        if (!Directory.Exists(options.LoadtestDir))
        {
            Console.Error.WriteLine($"Loadtest directory not found: {options.LoadtestDir}");
            return 2;
        }

        if (!File.Exists(roMixPath) || !File.Exists(rwMixPath))
        {
            Console.Error.WriteLine("Mix files not found. Expected:");
            Console.Error.WriteLine($"  {roMixPath}");
            Console.Error.WriteLine($"  {rwMixPath}");
            return 2;
        }

        if (!Directory.Exists(snippetDir))
        {
            Console.Error.WriteLine($"Snippet directory not found: {snippetDir}");
            return 2;
        }

        List<MixEntry> roEntries;
        List<MixEntry> rwEntries;
        TxBundleQueries txQueries;
        try
        {
            roEntries = MixLoader.LoadMix(roMixPath, snippetDir);
            rwEntries = MixLoader.LoadMix(rwMixPath, snippetDir);
            txQueries = TxBundleQueries.Load(snippetDir);
        }
        catch (Exception ex)
        {
            Console.Error.WriteLine($"Failed to load mix/snippet files: {ex.Message}");
            return 2;
        }

        var roPicker = new WeightedPicker(roEntries);
        var rwPicker = new WeightedPicker(rwEntries);

        var roConnString = ConnectionStrings.Build(
            options,
            applicationName: "commerce-lt-ro",
            minPool: options.RoMinPool,
            maxPool: options.RoMaxPool,
            multiplexing: options.RoMultiplexing,
            defaultReadOnly: true);

        var rwConnString = ConnectionStrings.Build(
            options,
            applicationName: "commerce-lt-rw",
            minPool: options.RwMinPool,
            maxPool: options.RwMaxPool,
            multiplexing: options.RwMultiplexing,
            defaultReadOnly: false);

        Console.WriteLine("Starting Npgsql load test with settings:");
        Console.WriteLine($"  LoadtestDir: {options.LoadtestDir}");
        Console.WriteLine($"  DB: {options.Host}:{options.Port}/{options.Database} user={options.Username}");
        Console.WriteLine($"  RO workers={options.RoWorkers}, ops/worker={options.RoOpsPerWorker}, pool(min/max)={options.RoMinPool}/{options.RoMaxPool}, multiplex={options.RoMultiplexing}");
        Console.WriteLine($"  RW workers={options.RwWorkers}, ops/worker={options.RwOpsPerWorker}, pool(min/max)={options.RwMinPool}/{options.RwMaxPool}, multiplex={options.RwMultiplexing}");
        Console.WriteLine($"  Think time ms={options.ThinkTimeMs}, command timeout s={options.CommandTimeoutSeconds}, continueOnError={options.ContinueOnError} (forced HA mode)");
        Console.WriteLine($"  TX retries max={options.TxBundleMaxRetries}, tx retry base delay ms={options.TxBundleRetryBaseDelayMs}");
        Console.WriteLine($"  Prepare seed={options.PrepareSeed}, auto-seed-if-empty={options.AutoSeedIfEmpty}");

        var metrics = new Metrics();

        using var cts = new CancellationTokenSource();
        Console.CancelKeyPress += (_, e) =>
        {
            e.Cancel = true;
            cts.Cancel();
            Console.Error.WriteLine("Cancellation requested (Ctrl+C). Stopping workers...");
        };

        var stopwatch = Stopwatch.StartNew();

        try
        {
            SeedPools seedPools;
            await using (var bootstrapDataSource = DataSourceFactory.Build(rwConnString))
            {
                if (options.PrepareSeed)
                {
                    await PrepareSeedAsync(bootstrapDataSource, options, cts.Token);
                }

                seedPools = await LoadSeedPoolsAsync(bootstrapDataSource, options, cts.Token);
                if (!seedPools.HasRequiredData && options.AutoSeedIfEmpty)
                {
                    Console.WriteLine("Seed pools incomplete; applying seed SQL and reloading key pools...");
                    await ExecuteSqlFileAsync(
                        bootstrapDataSource,
                        options.SeedSqlFile,
                        commandTimeoutSeconds: options.SeedCommandTimeoutSeconds,
                        cancellationToken: cts.Token);
                    seedPools = await LoadSeedPoolsAsync(bootstrapDataSource, options, cts.Token);
                }
            }

            seedPools.ValidateOrThrow();
            PrintSeedPoolStats(seedPools);

            await using var roDataSource = DataSourceFactory.Build(roConnString);
            await using var rwDataSource = DataSourceFactory.Build(rwConnString);

            using var progressCts = CancellationTokenSource.CreateLinkedTokenSource(cts.Token);
            var progressTask = ProgressLoopAsync(metrics, stopwatch, options.ProgressSeconds, progressCts.Token);

            List<WorkerReport> reports;
            if (options.IsPacedMode)
            {
                var duration = TimeSpan.FromSeconds(options.DurationSeconds);
                var roWeight = roEntries.Sum(static e => (double)e.Weight);
                var rwWeight = rwEntries.Sum(static e => (double)e.Weight);
                var totalWeight = roWeight + rwWeight;

                var roTargetTps = totalWeight <= 0 ? 0 : options.TargetTotalTps * (roWeight / totalWeight);
                var rwTargetTps = Math.Max(0, options.TargetTotalTps - roTargetTps);

                Console.WriteLine(
                    $"  Paced mode enabled: duration={options.DurationSeconds}s, target_total_tps={options.TargetTotalTps:F2}, " +
                    $"target_ro_tps={roTargetTps:F2}, target_rw_tps={rwTargetTps:F2}");

                var roChannel = Channel.CreateBounded<MixEntry>(new BoundedChannelOptions(options.ScheduleQueueCapacity)
                {
                    SingleWriter = true,
                    SingleReader = false,
                    FullMode = BoundedChannelFullMode.Wait
                });

                var rwChannel = Channel.CreateBounded<MixEntry>(new BoundedChannelOptions(options.ScheduleQueueCapacity)
                {
                    SingleWriter = true,
                    SingleReader = false,
                    FullMode = BoundedChannelFullMode.Wait
                });

                var producerTasks = new[]
                {
                    RunLaneProducerAsync(
                        lane: Lane.Ro,
                        entries: roEntries,
                        targetTps: roTargetTps,
                        duration: duration,
                        writer: roChannel.Writer,
                        options: options,
                        cancellationToken: cts.Token),
                    RunLaneProducerAsync(
                        lane: Lane.Rw,
                        entries: rwEntries,
                        targetTps: rwTargetTps,
                        duration: duration,
                        writer: rwChannel.Writer,
                        options: options,
                        cancellationToken: cts.Token)
                };

                var workerTasks = new List<Task<WorkerReport>>(options.RoWorkers + options.RwWorkers);
                for (var i = 1; i <= options.RoWorkers; i++)
                {
                    workerTasks.Add(RunScheduledWorkerAsync(
                        lane: Lane.Ro,
                        workerIndex: i,
                        reader: roChannel.Reader,
                        txQueries: txQueries,
                        seedPools: seedPools,
                        dataSource: roDataSource,
                        options: options,
                        metrics: metrics,
                        cts: cts));
                }

                for (var i = 1; i <= options.RwWorkers; i++)
                {
                    workerTasks.Add(RunScheduledWorkerAsync(
                        lane: Lane.Rw,
                        workerIndex: i,
                        reader: rwChannel.Reader,
                        txQueries: txQueries,
                        seedPools: seedPools,
                        dataSource: rwDataSource,
                        options: options,
                        metrics: metrics,
                        cts: cts));
                }

                await Task.WhenAll(producerTasks);
                reports = (await Task.WhenAll(workerTasks)).ToList();
            }
            else
            {
                var workerTasks = new List<Task<WorkerReport>>(options.RoWorkers + options.RwWorkers);

                for (var i = 1; i <= options.RoWorkers; i++)
                {
                    workerTasks.Add(RunWorkerAsync(
                        lane: Lane.Ro,
                        workerIndex: i,
                        operationCount: options.RoOpsPerWorker,
                        picker: roPicker,
                        txQueries: txQueries,
                        seedPools: seedPools,
                        dataSource: roDataSource,
                        options: options,
                        metrics: metrics,
                        cts: cts));
                }

                for (var i = 1; i <= options.RwWorkers; i++)
                {
                    workerTasks.Add(RunWorkerAsync(
                        lane: Lane.Rw,
                        workerIndex: i,
                        operationCount: options.RwOpsPerWorker,
                        picker: rwPicker,
                        txQueries: txQueries,
                        seedPools: seedPools,
                        dataSource: rwDataSource,
                        options: options,
                        metrics: metrics,
                        cts: cts));
                }

                reports = (await Task.WhenAll(workerTasks)).ToList();
            }

            progressCts.Cancel();
            try
            {
                await progressTask;
            }
            catch (OperationCanceledException)
            {
                // Expected.
            }

            stopwatch.Stop();

            var failedWorkers = reports.Where(r => r.Failed).ToList();
            PrintSummary(metrics, stopwatch.Elapsed, failedWorkers);

            return failedWorkers.Count == 0 && metrics.ErrorCount == 0 && !cts.IsCancellationRequested ? 0 : 1;
        }
        catch (OperationCanceledException)
        {
            stopwatch.Stop();
            Console.Error.WriteLine("Run canceled.");
            PrintSummary(metrics, stopwatch.Elapsed, failedWorkers: []);
            return 1;
        }
        catch (Exception ex)
        {
            stopwatch.Stop();
            Console.Error.WriteLine($"Fatal error: {ex.Message}");
            PrintSummary(metrics, stopwatch.Elapsed, failedWorkers: []);
            return 1;
        }
    }

    private static async Task PrepareSeedAsync(NpgsqlDataSource dataSource, RunnerOptions options, CancellationToken cancellationToken)
    {
        Console.WriteLine($"Preparing workload tables: reset={options.ResetSqlFile}, seed={options.SeedSqlFile}");

        await ExecuteSqlFileAsync(
            dataSource,
            options.ResetSqlFile,
            commandTimeoutSeconds: options.SeedCommandTimeoutSeconds,
            cancellationToken: cancellationToken);

        await ExecuteSqlFileAsync(
            dataSource,
            options.SeedSqlFile,
            commandTimeoutSeconds: options.SeedCommandTimeoutSeconds,
            cancellationToken: cancellationToken);
    }

    private static async Task ExecuteSqlFileAsync(
        NpgsqlDataSource dataSource,
        string sqlFile,
        int commandTimeoutSeconds,
        CancellationToken cancellationToken)
    {
        if (!File.Exists(sqlFile))
        {
            throw new FileNotFoundException($"SQL file not found: {sqlFile}");
        }

        var sql = await File.ReadAllTextAsync(sqlFile, cancellationToken);
        if (string.IsNullOrWhiteSpace(sql))
        {
            throw new InvalidOperationException($"SQL file is empty: {sqlFile}");
        }

        await using var connection = await dataSource.OpenConnectionAsync(cancellationToken);
        await using var command = connection.CreateCommand();
        command.CommandText = sql;
        command.CommandTimeout = commandTimeoutSeconds;
        await command.ExecuteNonQueryAsync(cancellationToken);
    }

    private static async Task<SeedPools> LoadSeedPoolsAsync(NpgsqlDataSource dataSource, RunnerOptions options, CancellationToken cancellationToken)
    {
        await using var connection = await dataSource.OpenConnectionAsync(cancellationToken);

        var sales = await QueryAsync(
            connection,
            "select id, createdbyid, organizationunitid from public.sales where regionid = 1",
            options.CommandTimeoutSeconds,
            r => new OrderSeed(r.GetGuid(0), r.GetGuid(1), r.IsDBNull(2) ? Guid.Empty : r.GetGuid(2)),
            cancellationToken);

        var invoiceIds = await QueryAsync(
            connection,
            "select id from public.billings where regionid = 1",
            options.CommandTimeoutSeconds,
            r => r.GetGuid(0),
            cancellationToken);

        var scheduledTaskIds = await QueryAsync(
            connection,
            "select id from public.backgroundjobs where regionid = 1",
            options.CommandTimeoutSeconds,
            r => r.GetString(0),
            cancellationToken);

        var savedlists = await QueryAsync(
            connection,
            "select id, userid, createdbyid from public.savedlists where regionid = 1 and isdeleted = false",
            options.CommandTimeoutSeconds,
            r => new WishlistSeed(r.GetGuid(0), r.IsDBNull(1) ? null : r.GetGuid(1), r.GetGuid(2)),
            cancellationToken);

        var savedlistdata = await QueryAsync(
            connection,
            "select wishlistid, type from public.savedlistdata where regionid = 1",
            options.CommandTimeoutSeconds,
            r => new WishlistDataSeed(r.GetGuid(0), r.GetString(1)),
            cancellationToken);

        var auditLocks = await QueryAsync(
            connection,
            "select id, typeid from public.eventchainlocks_1 where regionid = 1",
            options.CommandTimeoutSeconds,
            r => new AuditLockSeed(r.GetInt32(0), r.GetInt32(1)),
            cancellationToken);

        var userIds = await QueryAsync(
            connection,
            "select id from public.accounts where regionid = 1 and isdeleted = false",
            options.CommandTimeoutSeconds,
            r => r.GetGuid(0),
            cancellationToken);

        var stockLabelIds = await QueryAsync(
            connection,
            "select distinct stocklabelid from public.inventory_1 where regionid = 1 and stocklabelid is not null",
            options.CommandTimeoutSeconds,
            r => r.GetGuid(0),
            cancellationToken);

        var taxCodeIds = await QueryAsync(
            connection,
            "select distinct taxcodeid from public.saleslines where regionid = 1 and taxcodeid is not null",
            options.CommandTimeoutSeconds,
            r => r.GetGuid(0),
            cancellationToken);

        var organizationUnitIds = await QueryAsync(
            connection,
            "select id from public.branches where isdeleted = false",
            options.CommandTimeoutSeconds,
            r => r.GetGuid(0),
            cancellationToken);

        var userTaskTypeIds = await QueryAsync(
            connection,
            "select distinct typeid from public.accounttasks where regionid = 1 and isactive = true and iscompleted = false and userid is null and starttime is null and data is not null",
            options.CommandTimeoutSeconds,
            r => r.GetGuid(0),
            cancellationToken);

        var financialStatuses = await QueryAsync(
            connection,
            "select distinct statusid from public.accountingperiods where regionid = 1",
            options.CommandTimeoutSeconds,
            r => r.GetInt32(0),
            cancellationToken);

        var minApiKeyId = Guid.Empty;
        await using (var minApiKeyCmd = connection.CreateCommand())
        {
            minApiKeyCmd.CommandText = "select id from public.accesskeys order by id asc limit 1";
            minApiKeyCmd.CommandTimeout = options.CommandTimeoutSeconds;
            var scalar = await minApiKeyCmd.ExecuteScalarAsync(cancellationToken);
            if (scalar is Guid g)
            {
                minApiKeyId = g;
            }
        }

        var userTaskTypesFinal = userTaskTypeIds.Count == 0 ? [DefaultUserTaskTypeId] : userTaskTypeIds;
        var financialStatusesFinal = financialStatuses.Count == 0 ? [0, 1, 2] : financialStatuses;

        return new SeedPools(
            sales: sales,
            InvoiceIds: invoiceIds,
            ScheduledTaskIds: scheduledTaskIds,
            savedlists: savedlists,
            savedlistdata: savedlistdata,
            AuditLocks: auditLocks,
            UserIds: userIds,
            StockLabelIds: stockLabelIds,
            TaxCodeIds: taxCodeIds,
            OrganizationUnitIds: organizationUnitIds,
            UserTaskTypeIds: userTaskTypesFinal,
            FinancialStatuses: financialStatusesFinal,
            MinApiKeyId: minApiKeyId);
    }

    private static async Task<List<T>> QueryAsync<T>(
        NpgsqlConnection connection,
        string sql,
        int commandTimeoutSeconds,
        Func<NpgsqlDataReader, T> projector,
        CancellationToken cancellationToken)
    {
        var items = new List<T>();

        await using var command = connection.CreateCommand();
        command.CommandText = sql;
        command.CommandTimeout = commandTimeoutSeconds;

        await using var reader = await command.ExecuteReaderAsync(cancellationToken);
        while (await reader.ReadAsync(cancellationToken))
        {
            items.Add(projector(reader));
        }

        return items;
    }

    private static void PrintSeedPoolStats(SeedPools pools)
    {
        Console.WriteLine("Seed pool stats:");
        Console.WriteLine($"  sales={pools.sales.Count}, billings={pools.InvoiceIds.Count}, backgroundjobs={pools.ScheduledTaskIds.Count}");
        Console.WriteLine($"  savedlists={pools.savedlists.Count}, savedlistdata={pools.savedlistdata.Count}, eventchainlocks={pools.AuditLocks.Count}");
        Console.WriteLine($"  accounts={pools.UserIds.Count}, stocklabels={pools.StockLabelIds.Count}, taxcodes={pools.TaxCodeIds.Count}, orgunits={pools.OrganizationUnitIds.Count}");
    }

    private static async Task<WorkerReport> RunWorkerAsync(
        Lane lane,
        int workerIndex,
        int operationCount,
        WeightedPicker picker,
        TxBundleQueries txQueries,
        SeedPools seedPools,
        NpgsqlDataSource dataSource,
        RunnerOptions options,
        Metrics metrics,
        CancellationTokenSource cts)
    {
        var workerName = $"{lane.ToString().ToLowerInvariant()}-worker-{workerIndex:D2}";
        var random = new Random(RandomNumberGenerator.GetInt32(int.MaxValue));

        var completed = 0;
        var failed = false;

        NpgsqlConnection? connection = null;
        try
        {
            connection = await OpenConnectionWithRetryAsync(dataSource, workerName, cts.Token);

            for (var op = 1; op <= operationCount; op++)
            {
                if (cts.IsCancellationRequested)
                {
                    break;
                }

                var entry = picker.Next(random);
                var opSw = Stopwatch.StartNew();
                var retryTag = entry.Qid.ToUpperInvariant();
                var attempt = 0;

                while (true)
                {
                    attempt++;
                    try
                    {
                        await ExecuteEntryAsync(connection, entry, txQueries, seedPools, random, options, cts.Token);

                        opSw.Stop();
                        completed++;
                        metrics.RecordSuccess(lane, entry.Qid, opSw.Elapsed);
                        break;
                    }
                    catch (OperationCanceledException)
                    {
                        throw;
                    }
                    catch (Exception ex)
                    {
                        if (attempt <= options.TxBundleMaxRetries
                            && IsRetryableTxBundleError(ex)
                            && !cts.IsCancellationRequested)
                        {
                            metrics.RecordTxBundleRetry();
                            await TryRollbackAsync(connection, options.CommandTimeoutSeconds, cts.Token);

                            var delayMs = ComputeTxRetryDelayMs(attempt, options.TxBundleRetryBaseDelayMs);
                            Console.Error.WriteLine(
                                $"[{workerName}] op={op}/{operationCount} qid={retryTag} transient retryable error ({GetSqlState(ex)}), retry {attempt}/{options.TxBundleMaxRetries} in {delayMs}ms");

                            if (delayMs > 0)
                            {
                                try
                                {
                                    await Task.Delay(delayMs, cts.Token);
                                }
                                catch (OperationCanceledException)
                                {
                                    throw;
                                }
                            }

                            continue;
                        }

                        opSw.Stop();
                        metrics.RecordError(lane, entry.Qid, ex.Message);
                        Console.Error.WriteLine($"[{workerName}] op={op}/{operationCount} qid={entry.Qid} failed: {ex.Message}");
                        connection = await ReconnectAfterErrorAsync(connection, dataSource, workerName, cts.Token);
                        break;
                    }
                }

                if (options.ThinkTimeMs > 0)
                {
                    try
                    {
                        await Task.Delay(options.ThinkTimeMs, cts.Token);
                    }
                    catch (OperationCanceledException)
                    {
                        break;
                    }
                }
            }
        }
        catch (OperationCanceledException)
        {
            // Expected on cancellation.
        }
        finally
        {
            if (connection is not null)
            {
                await connection.DisposeAsync();
            }
        }

        return new WorkerReport(workerName, completed, failed);
    }

    private static async Task<WorkerReport> RunScheduledWorkerAsync(
        Lane lane,
        int workerIndex,
        ChannelReader<MixEntry> reader,
        TxBundleQueries txQueries,
        SeedPools seedPools,
        NpgsqlDataSource dataSource,
        RunnerOptions options,
        Metrics metrics,
        CancellationTokenSource cts)
    {
        var workerName = $"{lane.ToString().ToLowerInvariant()}-worker-{workerIndex:D2}";
        var random = new Random(RandomNumberGenerator.GetInt32(int.MaxValue));

        var completed = 0;
        var failed = false;

        NpgsqlConnection? connection = null;
        try
        {
            connection = await OpenConnectionWithRetryAsync(dataSource, workerName, cts.Token);

            while (!cts.IsCancellationRequested)
            {
                MixEntry entry;
                try
                {
                    if (!await reader.WaitToReadAsync(cts.Token))
                    {
                        break;
                    }

                    if (!reader.TryRead(out entry!))
                    {
                        continue;
                    }
                }
                catch (OperationCanceledException)
                {
                    break;
                }

                var opSw = Stopwatch.StartNew();
                var attempt = 0;
                var completedCurrent = false;

                while (!completedCurrent)
                {
                    attempt++;
                    try
                    {
                        await ExecuteEntryAsync(connection, entry, txQueries, seedPools, random, options, cts.Token);

                        opSw.Stop();
                        completed++;
                        metrics.RecordSuccess(lane, entry.Qid, opSw.Elapsed);
                        completedCurrent = true;
                    }
                    catch (OperationCanceledException)
                    {
                        throw;
                    }
                    catch (Exception ex)
                    {
                        if (attempt <= options.TxBundleMaxRetries
                            && IsRetryableTxBundleError(ex)
                            && !cts.IsCancellationRequested)
                        {
                            metrics.RecordTxBundleRetry();
                            await TryRollbackAsync(connection, options.CommandTimeoutSeconds, cts.Token);

                            var delayMs = ComputeTxRetryDelayMs(attempt, options.TxBundleRetryBaseDelayMs);
                            Console.Error.WriteLine(
                                $"[{workerName}] qid={entry.Qid} transient retryable error ({GetSqlState(ex)}), retry {attempt}/{options.TxBundleMaxRetries} in {delayMs}ms");

                            if (delayMs > 0)
                            {
                                try
                                {
                                    await Task.Delay(delayMs, cts.Token);
                                }
                                catch (OperationCanceledException)
                                {
                                    throw;
                                }
                            }

                            continue;
                        }

                        opSw.Stop();
                        metrics.RecordError(lane, entry.Qid, ex.Message);
                        Console.Error.WriteLine($"[{workerName}] qid={entry.Qid} failed: {ex.Message}");
                        connection = await ReconnectAfterErrorAsync(connection, dataSource, workerName, cts.Token);
                        completedCurrent = true;
                    }
                }
            }
        }
        catch (OperationCanceledException)
        {
            // Expected on cancellation.
        }
        finally
        {
            if (connection is not null)
            {
                await connection.DisposeAsync();
            }
        }

        return new WorkerReport(workerName, completed, failed);
    }

    private static async Task RunLaneProducerAsync(
        Lane lane,
        IReadOnlyList<MixEntry> entries,
        double targetTps,
        TimeSpan duration,
        ChannelWriter<MixEntry> writer,
        RunnerOptions options,
        CancellationToken cancellationToken)
    {
        if (targetTps <= 0 || entries.Count == 0)
        {
            writer.TryComplete();
            return;
        }

        var random = new Random(RandomNumberGenerator.GetInt32(int.MaxValue));
        var window = TimeSpan.FromSeconds(Math.Max(1, options.ScheduleWindowSeconds));
        var clock = Stopwatch.StartNew();

        try
        {
            while (clock.Elapsed < duration && !cancellationToken.IsCancellationRequested)
            {
                var windowStart = clock.Elapsed;
                var remaining = duration - windowStart;
                var activeWindow = remaining < window ? remaining : window;
                if (activeWindow <= TimeSpan.Zero)
                {
                    break;
                }

                var operationCount = (int)Math.Round(targetTps * activeWindow.TotalSeconds, MidpointRounding.AwayFromZero);
                if (operationCount <= 0)
                {
                    var idleDelay = windowStart + activeWindow - clock.Elapsed;
                    if (idleDelay > TimeSpan.Zero)
                    {
                        await Task.Delay(idleDelay, cancellationToken);
                    }

                    continue;
                }

                var batch = BuildLaneBatch(entries, operationCount, random);
                var spacingTicks = batch.Count > 0 ? activeWindow.Ticks / (double)batch.Count : 0;

                for (var i = 0; i < batch.Count; i++)
                {
                    var due = windowStart + TimeSpan.FromTicks((long)Math.Round((i + 1) * spacingTicks));
                    var delay = due - clock.Elapsed;
                    if (delay > TimeSpan.Zero)
                    {
                        await Task.Delay(delay, cancellationToken);
                    }

                    await writer.WriteAsync(batch[i], cancellationToken);
                }

                var windowRemainder = windowStart + activeWindow - clock.Elapsed;
                if (windowRemainder > TimeSpan.Zero)
                {
                    await Task.Delay(windowRemainder, cancellationToken);
                }
            }
        }
        catch (OperationCanceledException)
        {
            // Expected on cancellation.
        }
        finally
        {
            writer.TryComplete();
            Console.WriteLine($"  Producer completed for lane {lane} at elapsed={clock.Elapsed:hh\\:mm\\:ss}");
        }
    }

    private static List<MixEntry> BuildLaneBatch(IReadOnlyList<MixEntry> entries, int operationCount, Random random)
    {
        var n = entries.Count;
        var counts = new int[n];
        var fractions = new double[n];
        var totalWeight = entries.Sum(static e => (double)e.Weight);

        var assigned = 0;
        for (var i = 0; i < n; i++)
        {
            var exact = operationCount * (entries[i].Weight / totalWeight);
            var baseCount = (int)Math.Floor(exact);
            counts[i] = baseCount;
            fractions[i] = exact - baseCount;
            assigned += baseCount;
        }

        var remainder = operationCount - assigned;
        if (remainder > 0)
        {
            var ordered = Enumerable.Range(0, n)
                .OrderByDescending(i => fractions[i])
                .ThenBy(i => entries[i].Qid, StringComparer.Ordinal)
                .Take(remainder);

            foreach (var idx in ordered)
            {
                counts[idx]++;
            }
        }

        var batch = new List<MixEntry>(operationCount);
        for (var i = 0; i < n; i++)
        {
            for (var c = 0; c < counts[i]; c++)
            {
                batch.Add(entries[i]);
            }
        }

        for (var i = batch.Count - 1; i > 0; i--)
        {
            var j = random.Next(i + 1);
            (batch[i], batch[j]) = (batch[j], batch[i]);
        }

        return batch;
    }

    private static async Task ExecuteEntryAsync(
        NpgsqlConnection connection,
        MixEntry entry,
        TxBundleQueries txQueries,
        SeedPools seedPools,
        Random random,
        RunnerOptions options,
        CancellationToken cancellationToken)
    {
        var qid = entry.Qid.ToUpperInvariant();

        if (qid == "TXBUNDLE")
        {
            await ExecuteTxBundleAsync(connection, txQueries, seedPools, random, options, cancellationToken);
            return;
        }

        await using var command = connection.CreateCommand();
        command.CommandText = entry.Sql;
        command.CommandTimeout = options.CommandTimeoutSeconds;

        switch (qid)
        {
            case "Q01":
                AddParam(command, new[] { "sales", "billings" }, NpgsqlDbType.Array | NpgsqlDbType.Text);
                break;
            case "Q02":
            {
                var now = DateTime.UtcNow;
                AddParam(command, now.AddSeconds(30));
                AddParam(command, now);
                AddParam(command, seedPools.PickScheduledTaskId(random));
                AddParam(command, 0L);
                AddParam(command, 1L);
                AddParam(command, 0L);
                AddParam(command, 1L);
                break;
            }
            case "Q03":
            case "Q04":
            case "Q05":
            case "Q07":
                AddParam(command, seedPools.PickOrder(random).OrderId);
                AddParam(command, (short)1);
                break;
            case "Q06":
            case "Q08":
            case "Q16":
                AddParam(command, (short)1);
                AddParam(command, seedPools.PickInvoiceId(random));
                break;
            case "Q09":
            {
                var order = seedPools.PickOrder(random);
                var createdBy = order.CreatedById != Guid.Empty ? order.CreatedById : seedPools.PickUserId(random);
                var item = new SalesLedgerToInsert
                {
                    Id = NewUuidV7(),
                    Orderid = order.OrderId,
                    Orderlineid = null,
                    Typeid = NewUuidV7(),
                    Creationtime = DateTime.UtcNow,
                    Description = "lt-q09-ledger",
                    Oldvalue = null,
                    Newvalue = null,
                    Regionid = 1
                };

                AddParam(command, createdBy);
                AddParam(command, new[] { item });
                break;
            }
            case "Q10":
                AddParam(command, seedPools.PickScheduledTaskId(random));
                break;
            case "Q11":
            {
                var lockSeed = seedPools.PickAuditLock(random);
                AddParam(command, lockSeed.Id);
                AddParam(command, lockSeed.TypeId);
                break;
            }
            case "Q12":
            {
                var wishlist = seedPools.PickWishlist(random);
                AddParam(command, NewUuidV7());
                AddParam(command, (short)1);
                AddParam(command, NewUuidV7());
                AddParam(command, wishlist.WishlistId);
                AddParam(command, wishlist.CreatedById);
                AddParam(command, DateTime.UtcNow);
                break;
            }
            case "Q13":
                AddParam(command, (short)1);
                AddParam(command, seedPools.PickOrder(random).OrderId);
                break;
            case "Q14":
            {
                var order = seedPools.PickOrder(random);
                var now = DateTime.UtcNow;
                var userId = order.CreatedById != Guid.Empty ? order.CreatedById : seedPools.PickUserId(random);
                var identifier = order.OrderId.ToString();
                var currentStateJson = await ReadCurrentAuditStateAsync(connection, transaction: null, options.CommandTimeoutSeconds, identifier, cancellationToken) ?? "{}";
                var nextSequence = ExtractSequence(currentStateJson) + 1;
                var newStateJson = JsonSerializer.Serialize(new Dictionary<string, object>
                {
                    ["seq"] = nextSequence,
                    ["sig"] = CreateShortSignature()
                });

                AddParam(command, NewUuidV7());
                AddParam(command, (short)1);
                AddParam(command, identifier);
                AddParam(command, 1L);
                AddParam(command, 1);
                AddParam(command, 1);
                AddParam(command, currentStateJson, NpgsqlDbType.Jsonb);
                AddParam(command, newStateJson, NpgsqlDbType.Jsonb);
                AddParam(command, now);
                AddParam(command, userId);
                AddParam(command, NewUuidV7());
                break;
            }
            case "Q15":
                AddQ15Params(command);
                break;
            case "Q17":
            {
                var order = seedPools.PickOrder(random);
                var now = DateTime.UtcNow;
                var userId = order.CreatedById != Guid.Empty ? order.CreatedById : seedPools.PickUserId(random);
                var organizationUnitId = order.OrganizationUnitId != Guid.Empty ? order.OrganizationUnitId : seedPools.PickOrganizationUnitId(random);
                var lineId = NewUuidV7();
                var productId = NewUuidV7();
                var stockLabelId = seedPools.PickStockLabelId(random);
                var taxCodeId = seedPools.PickTaxCodeId(random);

                foreach (var value in BuildQ17Params(
                             lineId,
                             order.OrderId,
                             productId,
                             userId,
                             organizationUnitId,
                             stockLabelId,
                             taxCodeId,
                             now))
                {
                    AddParam(command, value);
                }

                break;
            }
            case "Q18":
            {
                var organizationUnitId = seedPools.PickOrganizationUnitId(random);
                var createdById = seedPools.PickUserId(random);
                var item = new ItemLedgerToInsert
                {
                    Id = NewUuidV7(),
                    Typeid = 1,
                    Productid = NewUuidV7(),
                    Organizationunitid = organizationUnitId,
                    Quantity = 1,
                    Quantityallocated = 0,
                    Remark = "lt-q18-ledger",
                    Stocklabelid = seedPools.PickStockLabelId(random),
                    Issummary = false,
                    Stockmutationid = NewUuidV7(),
                    Orderlineid = null,
                    Createdbyid = createdById,
                    Creationtime = DateTime.UtcNow
                };

                AddParam(command, (short)1);
                AddParam(command, new[] { item });
                AddParam(command, 1);
                AddParam(command, 2);
                break;
            }
            case "Q19":
            {
                var wishlist = seedPools.PickWishlist(random);
                AddParam(command, wishlist.WishlistId);
                AddParam(command, wishlist.UserId, NpgsqlDbType.Uuid);
                AddParam(command, 1);
                AddParam(command, false);
                AddParam(command, false);
                AddParam(command, false);
                AddParam(command, false);
                AddParam(command, false);
                break;
            }
            case "Q20":
                AddParam(command, seedPools.PickWishlist(random).WishlistId);
                break;
            case "Q22":
            {
                var wd = seedPools.PickWishlistData(random);
                AddParam(command, wd.WishlistId);
                AddParam(command, wd.Type);
                AddParam(command, (short)1);
                break;
            }
            case "Q23":
                AddParam(command, 1);
                AddParam(command, seedPools.PickUserTaskTypeId(random));
                AddParam(command, false);
                AddParam(command, true);
                AddParam(command, false);
                break;
            case "Q24":
                AddParam(command, DateTime.UtcNow);
                AddParam(command, seedPools.MinApiKeyId);
                AddParam(command, 100);
                AddParam(command, true);
                break;
            case "Q25":
                AddParam(command, "c");
                AddParam(command, "c");
                AddParam(command, 0);
                break;
            case "Q26":
                AddParam(command, seedPools.FinancialStatuses.ToArray(), NpgsqlDbType.Array | NpgsqlDbType.Integer);
                break;
            case "Q27":
                break;
            default:
                break;
        }

        await command.ExecuteNonQueryAsync(cancellationToken);
    }

    private static async Task ExecuteTxBundleAsync(
        NpgsqlConnection connection,
        TxBundleQueries txQueries,
        SeedPools seedPools,
        Random random,
        RunnerOptions options,
        CancellationToken cancellationToken)
    {
        var lockSeed = seedPools.PickAuditLock(random);
        var order = seedPools.PickOrder(random);
        var userId = order.CreatedById != Guid.Empty ? order.CreatedById : seedPools.PickUserId(random);
        var organizationUnitId = order.OrganizationUnitId != Guid.Empty ? order.OrganizationUnitId : seedPools.PickOrganizationUnitId(random);

        var lineId = NewUuidV7();
        var productId = NewUuidV7();
        var stockLabelId = seedPools.PickStockLabelId(random);
        var taxCodeId = seedPools.PickTaxCodeId(random);
        var now = DateTime.UtcNow;

        await using var tx = await connection.BeginTransactionAsync(cancellationToken);

        // Q11
        await using (var q11 = CreateCommand(connection, txQueries.Q11Sql, options.CommandTimeoutSeconds, tx))
        {
            AddParam(q11, lockSeed.Id);
            AddParam(q11, lockSeed.TypeId);
            await q11.ExecuteNonQueryAsync(cancellationToken);
        }

        // Q17
        await using (var q17 = CreateCommand(connection, txQueries.Q17Sql, options.CommandTimeoutSeconds, tx))
        {
            foreach (var value in BuildQ17Params(
                         lineId,
                         order.OrderId,
                         productId,
                         userId,
                         organizationUnitId,
                         stockLabelId,
                         taxCodeId,
                         now))
            {
                AddParam(q17, value);
            }

            await q17.ExecuteNonQueryAsync(cancellationToken);
        }

        // Q18
        var productLedgerItem = new ItemLedgerToInsert
        {
            Id = NewUuidV7(),
            Typeid = 1,
            Productid = productId,
            Organizationunitid = organizationUnitId,
            Quantity = 1,
            Quantityallocated = 0,
            Remark = "lt-rw-tx",
            Stocklabelid = stockLabelId,
            Issummary = false,
            Stockmutationid = NewUuidV7(),
            Orderlineid = lineId,
            Createdbyid = userId,
            Creationtime = now
        };

        await using (var q18 = CreateCommand(connection, txQueries.Q18Sql, options.CommandTimeoutSeconds, tx))
        {
            AddParam(q18, (short)1);
            AddParam(q18, new[] { productLedgerItem });
            AddParam(q18, 1);
            AddParam(q18, 2);
            await q18.ExecuteNonQueryAsync(cancellationToken);
        }

        // Q09
        var orderLedgerItem = new SalesLedgerToInsert
        {
            Id = NewUuidV7(),
            Orderid = order.OrderId,
            Orderlineid = lineId,
            Typeid = NewUuidV7(),
            Creationtime = now,
            Description = "lt-rw-tx-ledger",
            Oldvalue = null,
            Newvalue = null,
            Regionid = 1
        };

        await using (var q09 = CreateCommand(connection, txQueries.Q09Sql, options.CommandTimeoutSeconds, tx))
        {
            AddParam(q09, userId);
            AddParam(q09, new[] { orderLedgerItem });
            await q09.ExecuteNonQueryAsync(cancellationToken);
        }

        // Q14
        var identifier = order.OrderId.ToString();
        var currentStateJson = await ReadCurrentAuditStateAsync(connection, tx, options.CommandTimeoutSeconds, identifier, cancellationToken) ?? "{}";
        var nextSequence = ExtractSequence(currentStateJson) + 1;
        var newStateJson = JsonSerializer.Serialize(new Dictionary<string, object>
        {
            ["seq"] = nextSequence,
            ["sig"] = CreateShortSignature()
        });

        await using (var q14 = CreateCommand(connection, txQueries.Q14Sql, options.CommandTimeoutSeconds, tx))
        {
            AddParam(q14, NewUuidV7());
            AddParam(q14, (short)1);
            AddParam(q14, identifier);
            AddParam(q14, 1L);
            AddParam(q14, 1);
            AddParam(q14, 1);
            AddParam(q14, currentStateJson, NpgsqlDbType.Jsonb);
            AddParam(q14, newStateJson, NpgsqlDbType.Jsonb);
            AddParam(q14, now);
            AddParam(q14, userId);
            AddParam(q14, NewUuidV7());
            await q14.ExecuteNonQueryAsync(cancellationToken);
        }

        await tx.CommitAsync(cancellationToken);
    }

    private static async Task<string?> ReadCurrentAuditStateAsync(
        NpgsqlConnection connection,
        NpgsqlTransaction? transaction,
        int commandTimeoutSeconds,
        string identifier,
        CancellationToken cancellationToken)
    {
        await using var command = connection.CreateCommand();
        if (transaction is not null)
        {
            command.Transaction = transaction;
        }
        command.CommandText = @"
            select state::text
            from public.eventchains_1
            where identifier = $1
              and discriminator = 1
              and typeid = 1
              and version = 1
              and isoffline = false
              and organizationunitid = empty_uuid()
              and regionid = 1
            limit 1;";
        command.CommandTimeout = commandTimeoutSeconds;
        AddParam(command, identifier);

        var scalar = await command.ExecuteScalarAsync(cancellationToken);
        return scalar as string;
    }

    private static long ExtractSequence(string json)
    {
        try
        {
            using var doc = JsonDocument.Parse(json);
            if (doc.RootElement.ValueKind == JsonValueKind.Object
                && doc.RootElement.TryGetProperty("seq", out var seq))
            {
                return seq.ValueKind switch
                {
                    JsonValueKind.Number => seq.GetInt64(),
                    JsonValueKind.String when long.TryParse(seq.GetString(), NumberStyles.Integer, CultureInfo.InvariantCulture, out var value) => value,
                    _ => 0
                };
            }
        }
        catch
        {
            // Keep default.
        }

        return 0;
    }

    private static string CreateShortSignature() => Convert.ToHexString(RandomNumberGenerator.GetBytes(8)).ToLowerInvariant();

    private static Guid NewUuidV7() => Guid.CreateVersion7();

    private static object?[] BuildQ17Params(
        Guid lineId,
        Guid orderId,
        Guid productId,
        Guid createdById,
        Guid organizationUnitId,
        Guid stockLabelId,
        Guid taxCodeId,
        DateTime creationTimeUtc)
    {
        const decimal unitPrice = 9.99m;

        return
        [
            lineId,                          // 1 id
            (short)1,                        // 2 regionid
            "lt-rw",                        // 3 additionaldescription
            0m,                              // 4 amountcorrectedafterinvoice
            0m,                              // 5 amountinvoiced
            DBNull.Value,                    // 6 appointmentid
            DBNull.Value,                    // 7 availabilitystatusid
            DBNull.Value,                    // 8 backendreference
            DBNull.Value,                    // 9 cancellationreason
            DBNull.Value,                    // 10 cardtransactiondate
            DBNull.Value,                    // 11 cardtransactionid
            DBNull.Value,                    // 12 cardtransactionstatusid
            0,                               // 13 commitmentstatusid
            DBNull.Value,                    // 14 contractnumber
            "EUR",                          // 15 costpricecurrencyid
            "EUR",                          // 16 currencyid
            DBNull.Value,                    // 17 customorderlinetype
            DBNull.Value,                    // 18 customstatusid
            DBNull.Value,                    // 19 data
            "lt-rw-tx-line",                // 20 description
            0m,                              // 21 discountamount
            0m,                              // 22 discountamountinvoiced
            DBNull.Value,                    // 23 discountcouponcode
            DBNull.Value,                    // 24 discountcouponid
            DBNull.Value,                    // 25 discountid
            DBNull.Value,                    // 26 discountpercentage
            DBNull.Value,                    // 27 ecotax
            1.0m,                            // 28 exchangerate
            DBNull.Value,                    // 29 expectedavailabilitydate
            0,                               // 30 exportstatusid
            0,                               // 31 externalmodificationstatusid
            DBNull.Value,                    // 32 externalsystem
            DBNull.Value,                    // 33 fulfillmentcalculationdate
            organizationUnitId,              // 34 fulfillmentorganizationunitid
            0,                               // 35 fulfillmentstatusid
            DBNull.Value,                    // 36 fulfillmentvisibilitygroupid
            DBNull.Value,                    // 37 groupid
            DBNull.Value,                    // 38 imageblobid
            false,                           // 39 iscancelled
            false,                           // 40 iscompleted
            false,                           // 41 isinvoiced
            false,                           // 42 isshipped
            DBNull.Value,                    // 43 ledgerclassid
            1,                               // 44 lineactiontypeid
            unitPrice,                       // 45 nettotalamount
            unitPrice,                       // 46 nettotalamountintax
            0m,                              // 47 nettotaltaxamount
            DBNull.Value,                    // 48 oldreturnreasonid
            0,                               // 49 optionsid
            orderId,                         // 50 orderid
            0,                               // 51 ordertypeid
            unitPrice,                       // 52 originalunitprice
            DBNull.Value,                    // 53 parentid
            0,                               // 54 placementstatusid
            DBNull.Value,                    // 55 presaledate
            productId,                       // 56 productid
            DBNull.Value,                    // 57 productbundlelineid
            DBNull.Value,                    // 58 productvariationdata
            0,                               // 59 quantitycancelled
            DBNull.Value,                    // 60 quantitycommitted
            0,                               // 61 quantitydelivered
            0,                               // 62 quantityexported
            0,                               // 63 quantityinvoiced
            1,                               // 64 quantityopen
            1,                               // 65 quantityordered
            0,                               // 66 quantityreserved
            0,                               // 67 quantityreturned
            0,                               // 68 quantityshipped
            1,                               // 69 quantitytoreserve
            1,                               // 70 quantitytoship
            DBNull.Value,                    // 71 reference
            DBNull.Value,                    // 72 refundcorrectionreasonid
            DBNull.Value,                    // 73 relatedorderlineid
            "lt-rw-tx",                     // 74 remark
            DBNull.Value,                    // 75 requesteddate
            DBNull.Value,                    // 76 reservationexpirationdate
            DBNull.Value,                    // 77 returnedorderid
            DBNull.Value,                    // 78 returnedorderlineid
            DBNull.Value,                    // 79 returnreasonid
            DBNull.Value,                    // 80 serialnumber
            DBNull.Value,                    // 81 serialnumberid
            DBNull.Value,                    // 82 shippingmethodid
            DBNull.Value,                    // 83 soldbyid
            stockLabelId,                    // 84 stocklabelid
            DBNull.Value,                    // 85 stockproductid
            0m,                              // 86 taxamount
            0m,                              // 87 taxamountinvoiced
            taxCodeId,                       // 88 taxcodeid
            DBNull.Value,                    // 89 taxexemptioncode
            DBNull.Value,                    // 90 taxexemptionreason
            DBNull.Value,                    // 91 taxexemptionverification
            0m,                              // 92 taxrate
            DBNull.Value,                    // 93 taxratecategoryid
            unitPrice,                       // 94 totalamount
            unitPrice,                       // 95 totalamountintax
            0m,                              // 96 totaltaxamount
            1,                               // 97 typeid
            DBNull.Value,                    // 98 unitcost
            unitPrice,                       // 99 unitprice
            unitPrice,                       // 100 unitpricebeforecorrection
            unitPrice,                       // 101 unitpricebeforecorrectionintax
            DBNull.Value,                    // 102 unitpricecorrectionreasonid
            unitPrice,                       // 103 unitpriceintax
            DBNull.Value,                    // 104 unitpricepricelistid
            0m,                              // 105 unittaxamount
            createdById,                     // 106 createdbyid
            creationTimeUtc                  // 107 creationtime
        ];
    }

    private static NpgsqlCommand CreateCommand(
        NpgsqlConnection connection,
        string sql,
        int commandTimeoutSeconds,
        NpgsqlTransaction? transaction = null)
    {
        var command = connection.CreateCommand();
        command.CommandText = sql;
        command.CommandTimeout = commandTimeoutSeconds;
        if (transaction is not null)
        {
            command.Transaction = transaction;
        }

        return command;
    }

    private static void AddParam(NpgsqlCommand command, object? value, NpgsqlDbType? dbType = null)
    {
        var parameter = new NpgsqlParameter
        {
            Value = value ?? DBNull.Value
        };

        if (dbType.HasValue)
        {
            parameter.NpgsqlDbType = dbType.Value;
        }

        command.Parameters.Add(parameter);
    }

    private static void AddQ15Params(NpgsqlCommand command)
    {
        AddParam(command, "array_recv");
        AddParam(command, "a");
        AddParam(command, "array_recv");
        AddParam(command, "a");
        AddParam(command, "array_recv");
        AddParam(command, "r");
        AddParam(command, "m");
        AddParam(command, "d");
        AddParam(command, "b");
        AddParam(command, "r");
        AddParam(command, "m");
        AddParam(command, "e");
        AddParam(command, "d");
        AddParam(command, "c");
        AddParam(command, "c");
        AddParam(command, "p");
        AddParam(command, "record");
        AddParam(command, "void");
        AddParam(command, "unknown");
        AddParam(command, "a");
        AddParam(command, "b");
        AddParam(command, "r");
        AddParam(command, "m");
        AddParam(command, "e");
        AddParam(command, "d");
        AddParam(command, "p");
        AddParam(command, "record");
        AddParam(command, "void");
        AddParam(command, "c");
        AddParam(command, "c");
        AddParam(command, "b");
        AddParam(command, "e");
        AddParam(command, "p");
        AddParam(command, 0);
        AddParam(command, "c");
        AddParam(command, 1);
        AddParam(command, "r");
        AddParam(command, 2);
        AddParam(command, "m");
        AddParam(command, 3);
        AddParam(command, "d");
        AddParam(command, "a");
        AddParam(command, 4);
        AddParam(command, "a");
        AddParam(command, 5);
        AddParam(command, "d");
        AddParam(command, "a");
        AddParam(command, 6);
    }

    private static bool IsRetryableTxBundleError(Exception ex)
    {
        if (ex is PostgresException pg)
        {
            return pg.SqlState is PostgresErrorCodes.SerializationFailure or PostgresErrorCodes.DeadlockDetected;
        }

        return false;
    }

    private static string GetSqlState(Exception ex) => ex is PostgresException pg ? pg.SqlState : "n/a";

    private static async Task TryRollbackAsync(NpgsqlConnection connection, int commandTimeoutSeconds, CancellationToken cancellationToken)
    {
        try
        {
            await using var command = connection.CreateCommand();
            command.CommandText = "ROLLBACK;";
            command.CommandTimeout = commandTimeoutSeconds;
            await command.ExecuteNonQueryAsync(cancellationToken);
        }
        catch
        {
            // Ignore rollback errors; next attempt will still run and surface hard failure if needed.
        }
    }

    private static async Task<NpgsqlConnection> OpenConnectionWithRetryAsync(
        NpgsqlDataSource dataSource,
        string workerName,
        CancellationToken cancellationToken)
    {
        var attempt = 0;

        while (true)
        {
            cancellationToken.ThrowIfCancellationRequested();
            attempt++;

            try
            {
                return await dataSource.OpenConnectionAsync(cancellationToken);
            }
            catch (Exception ex) when (ex is not OperationCanceledException)
            {
                var delayMs = ComputeReconnectDelayMs(attempt);
                Console.Error.WriteLine($"[{workerName}] connection open failed: {ex.Message}; reconnecting in {delayMs}ms");
                if (delayMs > 0)
                {
                    await Task.Delay(delayMs, cancellationToken);
                }
            }
        }
    }

    private static async Task<NpgsqlConnection> ReconnectAfterErrorAsync(
        NpgsqlConnection? connection,
        NpgsqlDataSource dataSource,
        string workerName,
        CancellationToken cancellationToken)
    {
        if (connection is not null)
        {
            try
            {
                await connection.DisposeAsync();
            }
            catch
            {
                // Ignore dispose errors and continue reconnect.
            }
        }

        return await OpenConnectionWithRetryAsync(dataSource, workerName, cancellationToken);
    }

    private static int ComputeTxRetryDelayMs(int attempt, int baseDelayMs)
    {
        var baseValue = Math.Max(1, baseDelayMs);
        var exponent = Math.Min(attempt - 1, 6);
        var multiplier = 1 << Math.Max(0, exponent);
        return baseValue * multiplier;
    }

    private static int ComputeReconnectDelayMs(int attempt)
    {
        var exponent = Math.Min(Math.Max(0, attempt - 1), 7);
        var multiplier = 1 << exponent;
        var delay = ReconnectBaseDelayMs * multiplier;
        return Math.Min(delay, ReconnectMaxDelayMs);
    }

    private static async Task ProgressLoopAsync(Metrics metrics, Stopwatch stopwatch, int progressSeconds, CancellationToken cancellationToken)
    {
        if (progressSeconds <= 0)
        {
            return;
        }

        while (!cancellationToken.IsCancellationRequested)
        {
            await Task.Delay(TimeSpan.FromSeconds(progressSeconds), cancellationToken);
            var elapsedSeconds = Math.Max(0.001, stopwatch.Elapsed.TotalSeconds);

            Console.WriteLine(
                $"progress elapsed={stopwatch.Elapsed:hh\\:mm\\:ss} ro={metrics.RoSuccessCount} rw={metrics.RwSuccessCount} " +
                $"errors={metrics.ErrorCount} tx_retries={metrics.TxBundleRetryCount} total_tps={(metrics.TotalSuccessCount / elapsedSeconds):F2}");
        }
    }

    private static void PrintSummary(Metrics metrics, TimeSpan elapsed, IReadOnlyCollection<WorkerReport> failedWorkers)
    {
        var elapsedSeconds = Math.Max(0.001, elapsed.TotalSeconds);

        Console.WriteLine();
        Console.WriteLine("=== Npgsql Load Test Summary ===");
        Console.WriteLine($"Elapsed: {elapsed:hh\\:mm\\:ss}");
        Console.WriteLine($"RO success ops: {metrics.RoSuccessCount} (tps={(metrics.RoSuccessCount / elapsedSeconds):F2})");
        Console.WriteLine($"RW success ops: {metrics.RwSuccessCount} (tps={(metrics.RwSuccessCount / elapsedSeconds):F2})");
        Console.WriteLine($"Total success ops: {metrics.TotalSuccessCount} (tps={(metrics.TotalSuccessCount / elapsedSeconds):F2})");
        Console.WriteLine($"Total errors: {metrics.ErrorCount}");
        Console.WriteLine($"TXBUNDLE retries: {metrics.TxBundleRetryCount}");

        if (metrics.RoSuccessCount > 0)
        {
            Console.WriteLine($"RO mean latency ms: {metrics.GetMeanLatencyMs(Lane.Ro):F3}");
        }

        if (metrics.RwSuccessCount > 0)
        {
            Console.WriteLine($"RW mean latency ms: {metrics.GetMeanLatencyMs(Lane.Rw):F3}");
        }

        if (failedWorkers.Count > 0)
        {
            Console.WriteLine("Failed workers:");
            foreach (var report in failedWorkers)
            {
                Console.WriteLine($"  {report.WorkerName} completed_ops={report.CompletedOperations}");
            }
        }

        Console.WriteLine();
        Console.WriteLine("QID execution counts:");
        foreach (var kvp in metrics.GetQidCounts().OrderByDescending(k => k.Value).ThenBy(k => k.Key))
        {
            Console.WriteLine($"  {kvp.Key}: {kvp.Value}");
        }

        if (metrics.ErrorCount > 0)
        {
            Console.WriteLine();
            Console.WriteLine("QID error counts:");
            foreach (var kvp in metrics.GetQidErrors().OrderByDescending(k => k.Value).ThenBy(k => k.Key))
            {
                Console.WriteLine($"  {kvp.Key}: {kvp.Value}");
            }
        }
    }
}

internal enum Lane
{
    Ro,
    Rw
}

internal sealed record WorkerReport(string WorkerName, int CompletedOperations, bool Failed);
internal sealed record MixEntry(string Qid, int Weight, string SnippetFile, string Sql);
internal sealed record OrderSeed(Guid OrderId, Guid CreatedById, Guid OrganizationUnitId);
internal sealed record WishlistSeed(Guid WishlistId, Guid? UserId, Guid CreatedById);
internal sealed record WishlistDataSeed(Guid WishlistId, string Type);
internal sealed record AuditLockSeed(int Id, int TypeId);

internal sealed class SeedPools(
    IReadOnlyList<OrderSeed> sales,
    IReadOnlyList<Guid> InvoiceIds,
    IReadOnlyList<string> ScheduledTaskIds,
    IReadOnlyList<WishlistSeed> savedlists,
    IReadOnlyList<WishlistDataSeed> savedlistdata,
    IReadOnlyList<AuditLockSeed> AuditLocks,
    IReadOnlyList<Guid> UserIds,
    IReadOnlyList<Guid> StockLabelIds,
    IReadOnlyList<Guid> TaxCodeIds,
    IReadOnlyList<Guid> OrganizationUnitIds,
    IReadOnlyList<Guid> UserTaskTypeIds,
    IReadOnlyList<int> FinancialStatuses,
    Guid MinApiKeyId)
{
    public IReadOnlyList<OrderSeed> sales { get; } = sales;
    public IReadOnlyList<Guid> InvoiceIds { get; } = InvoiceIds;
    public IReadOnlyList<string> ScheduledTaskIds { get; } = ScheduledTaskIds;
    public IReadOnlyList<WishlistSeed> savedlists { get; } = savedlists;
    public IReadOnlyList<WishlistDataSeed> savedlistdata { get; } = savedlistdata;
    public IReadOnlyList<AuditLockSeed> AuditLocks { get; } = AuditLocks;
    public IReadOnlyList<Guid> UserIds { get; } = UserIds;
    public IReadOnlyList<Guid> StockLabelIds { get; } = StockLabelIds;
    public IReadOnlyList<Guid> TaxCodeIds { get; } = TaxCodeIds;
    public IReadOnlyList<Guid> OrganizationUnitIds { get; } = OrganizationUnitIds;
    public IReadOnlyList<Guid> UserTaskTypeIds { get; } = UserTaskTypeIds;
    public IReadOnlyList<int> FinancialStatuses { get; } = FinancialStatuses;
    public Guid MinApiKeyId { get; } = MinApiKeyId;

    public bool HasRequiredData =>
        sales.Count > 0
        && InvoiceIds.Count > 0
        && ScheduledTaskIds.Count > 0
        && savedlists.Count > 0
        && savedlistdata.Count > 0
        && AuditLocks.Count > 0
        && UserIds.Count > 0
        && StockLabelIds.Count > 0
        && TaxCodeIds.Count > 0
        && OrganizationUnitIds.Count > 0;

    public void ValidateOrThrow()
    {
        var missing = new List<string>();

        if (sales.Count == 0) missing.Add("sales");
        if (InvoiceIds.Count == 0) missing.Add("billings");
        if (ScheduledTaskIds.Count == 0) missing.Add("backgroundjobs");
        if (savedlists.Count == 0) missing.Add("savedlists");
        if (savedlistdata.Count == 0) missing.Add("savedlistdata");
        if (AuditLocks.Count == 0) missing.Add("eventchainlocks_1");
        if (UserIds.Count == 0) missing.Add("accounts");
        if (StockLabelIds.Count == 0) missing.Add("inventory_1.stocklabelid");
        if (TaxCodeIds.Count == 0) missing.Add("saleslines.taxcodeid");
        if (OrganizationUnitIds.Count == 0) missing.Add("branches");

        if (missing.Count > 0)
        {
            throw new InvalidOperationException(
                "Seed pools are missing required data: " + string.Join(", ", missing) +
                ". Run with --prepare-seed or apply loadtest/sql/seed_happy_path.sql.");
        }
    }

    public OrderSeed PickOrder(Random random) => sales[random.Next(sales.Count)];
    public Guid PickInvoiceId(Random random) => InvoiceIds[random.Next(InvoiceIds.Count)];
    public string PickScheduledTaskId(Random random) => ScheduledTaskIds[random.Next(ScheduledTaskIds.Count)];
    public WishlistSeed PickWishlist(Random random) => savedlists[random.Next(savedlists.Count)];
    public WishlistDataSeed PickWishlistData(Random random) => savedlistdata[random.Next(savedlistdata.Count)];
    public AuditLockSeed PickAuditLock(Random random) => AuditLocks[random.Next(AuditLocks.Count)];
    public Guid PickUserId(Random random) => UserIds[random.Next(UserIds.Count)];
    public Guid PickStockLabelId(Random random) => StockLabelIds[random.Next(StockLabelIds.Count)];
    public Guid PickTaxCodeId(Random random) => TaxCodeIds[random.Next(TaxCodeIds.Count)];
    public Guid PickOrganizationUnitId(Random random) => OrganizationUnitIds[random.Next(OrganizationUnitIds.Count)];
    public Guid PickUserTaskTypeId(Random random) => UserTaskTypeIds[random.Next(UserTaskTypeIds.Count)];
}

internal sealed class WeightedPicker
{
    private readonly MixEntry[] _entries;
    private readonly long[] _cumulativeWeights;
    private readonly long _totalWeight;

    public WeightedPicker(IReadOnlyList<MixEntry> entries)
    {
        if (entries.Count == 0)
        {
            throw new ArgumentException("Entry list cannot be empty.", nameof(entries));
        }

        _entries = entries.ToArray();
        _cumulativeWeights = new long[_entries.Length];

        long running = 0;
        for (var i = 0; i < _entries.Length; i++)
        {
            if (_entries[i].Weight <= 0)
            {
                throw new ArgumentException($"Weight must be > 0 for {_entries[i].Qid}");
            }

            running += _entries[i].Weight;
            _cumulativeWeights[i] = running;
        }

        _totalWeight = running;
    }

    public MixEntry Next(Random random)
    {
        var ticket = random.NextInt64(1, _totalWeight + 1);
        var idx = Array.BinarySearch(_cumulativeWeights, ticket);
        if (idx < 0)
        {
            idx = ~idx;
        }

        return _entries[idx];
    }
}

internal static class MixLoader
{
    public static List<MixEntry> LoadMix(string mixPath, string snippetDir)
    {
        var results = new List<MixEntry>();

        foreach (var rawLine in File.ReadLines(mixPath))
        {
            var line = rawLine.Trim();
            if (line.Length == 0 || line.StartsWith('#'))
            {
                continue;
            }

            var parts = line.Split('\t', StringSplitOptions.None);
            if (parts.Length < 3)
            {
                throw new InvalidOperationException($"Invalid mix line in {mixPath}: {rawLine}");
            }

            var qid = parts[0].Trim();
            if (!int.TryParse(parts[1].Trim(), NumberStyles.Integer, CultureInfo.InvariantCulture, out var weight) || weight <= 0)
            {
                throw new InvalidOperationException($"Invalid weight in {mixPath}: {rawLine}");
            }

            var snippetFile = parts[2].Trim();
            var snippetPath = Path.Combine(snippetDir, snippetFile);
            if (!File.Exists(snippetPath))
            {
                throw new FileNotFoundException($"Snippet file for {qid} not found: {snippetPath}");
            }

            var sql = File.ReadAllText(snippetPath).Trim();
            if (string.IsNullOrWhiteSpace(sql))
            {
                throw new InvalidOperationException($"Snippet SQL is empty: {snippetPath}");
            }

            results.Add(new MixEntry(qid, weight, snippetFile, sql));
        }

        return results;
    }
}

internal sealed record TxBundleQueries(
    string Q09Sql,
    string Q11Sql,
    string Q14Sql,
    string Q17Sql,
    string Q18Sql)
{
    public static TxBundleQueries Load(string snippetDir)
    {
        string Load(string file)
        {
            var path = Path.Combine(snippetDir, file);
            if (!File.Exists(path))
            {
                throw new FileNotFoundException($"TX snippet not found: {path}");
            }

            var sql = File.ReadAllText(path).Trim();
            if (sql.Length == 0)
            {
                throw new InvalidOperationException($"TX snippet is empty: {path}");
            }

            return sql;
        }

        return new TxBundleQueries(
            Q09Sql: Load("q09_tx_insert_orderledger.sql"),
            Q11Sql: Load("q11_tx_select_auditchainlocks_for_update.sql"),
            Q14Sql: Load("q14_tx_upsert_auditchain_and_insert_ledger.sql"),
            Q17Sql: Load("q17_tx_insert_orderlines_wide.sql"),
            Q18Sql: Load("q18_tx_insert_productledger_and_upsert_stock.sql"));
    }
}

internal static class DataSourceFactory
{
    public static NpgsqlDataSource Build(string connectionString)
    {
        var builder = new NpgsqlDataSourceBuilder(connectionString);
        builder.MapComposite<SalesLedgerToInsert>("public.salesledgertoinsert");
        builder.MapComposite<ItemLedgerToInsert>("public.itemledgertoinsert");
        return builder.Build();
    }
}

internal sealed class SalesLedgerToInsert
{
    public Guid Id { get; init; }
    public Guid Orderid { get; init; }
    public Guid? Orderlineid { get; init; }
    public Guid Typeid { get; init; }
    public DateTime Creationtime { get; init; }
    public string? Description { get; init; }
    public string? Oldvalue { get; init; }
    public string? Newvalue { get; init; }
    public short Regionid { get; init; }
}

internal sealed class ItemLedgerToInsert
{
    public Guid Id { get; init; }
    public int Typeid { get; init; }
    public Guid Productid { get; init; }
    public Guid Organizationunitid { get; init; }
    public int Quantity { get; init; }
    public int Quantityallocated { get; init; }
    public string? Remark { get; init; }
    public Guid Stocklabelid { get; init; }
    public bool Issummary { get; init; }
    public Guid Stockmutationid { get; init; }
    public Guid? Orderlineid { get; init; }
    public Guid Createdbyid { get; init; }
    public DateTime Creationtime { get; init; }
}

internal static class ConnectionStrings
{
    public static string Build(
        RunnerOptions options,
        string applicationName,
        int minPool,
        int maxPool,
        bool multiplexing,
        bool defaultReadOnly)
    {
        var builder = new NpgsqlConnectionStringBuilder
        {
            Host = options.Host,
            Port = options.Port,
            Database = options.Database,
            Username = options.Username,
            Pooling = true,
            MinPoolSize = minPool,
            MaxPoolSize = maxPool,
            ConnectionLifetime = 1800,
            Timeout = 15,
            CommandTimeout = options.CommandTimeoutSeconds,
            ApplicationName = applicationName
        };

        if (!string.IsNullOrEmpty(options.Password))
        {
            builder.Password = options.Password;
        }

        builder["Multiplexing"] = multiplexing;
        builder["Options"] = defaultReadOnly ? "-c default_transaction_read_only=on" : "-c default_transaction_read_only=off";

        return builder.ConnectionString;
    }
}

internal sealed class Metrics
{
    private long _roSuccessCount;
    private long _rwSuccessCount;
    private long _errorCount;
    private long _txBundleRetryCount;

    private long _roLatencyTicks;
    private long _rwLatencyTicks;

    private readonly ConcurrentDictionary<string, long> _qidCounts = new(StringComparer.OrdinalIgnoreCase);
    private readonly ConcurrentDictionary<string, long> _qidErrors = new(StringComparer.OrdinalIgnoreCase);

    public long RoSuccessCount => Interlocked.Read(ref _roSuccessCount);
    public long RwSuccessCount => Interlocked.Read(ref _rwSuccessCount);
    public long ErrorCount => Interlocked.Read(ref _errorCount);
    public long TxBundleRetryCount => Interlocked.Read(ref _txBundleRetryCount);
    public long TotalSuccessCount => RoSuccessCount + RwSuccessCount;

    public void RecordSuccess(Lane lane, string qid, TimeSpan elapsed)
    {
        if (lane == Lane.Ro)
        {
            Interlocked.Increment(ref _roSuccessCount);
            Interlocked.Add(ref _roLatencyTicks, elapsed.Ticks);
        }
        else
        {
            Interlocked.Increment(ref _rwSuccessCount);
            Interlocked.Add(ref _rwLatencyTicks, elapsed.Ticks);
        }

        _qidCounts.AddOrUpdate(qid, 1, static (_, value) => value + 1);
    }

    public void RecordError(Lane lane, string qid, string _message)
    {
        Interlocked.Increment(ref _errorCount);
        _qidErrors.AddOrUpdate(qid, 1, static (_, value) => value + 1);
    }

    public void RecordTxBundleRetry()
    {
        Interlocked.Increment(ref _txBundleRetryCount);
    }

    public double GetMeanLatencyMs(Lane lane)
    {
        var count = lane == Lane.Ro ? RoSuccessCount : RwSuccessCount;
        if (count == 0)
        {
            return 0;
        }

        var ticks = lane == Lane.Ro
            ? Interlocked.Read(ref _roLatencyTicks)
            : Interlocked.Read(ref _rwLatencyTicks);

        return TimeSpan.FromTicks(ticks).TotalMilliseconds / count;
    }

    public IReadOnlyDictionary<string, long> GetQidCounts() => _qidCounts;

    public IReadOnlyDictionary<string, long> GetQidErrors() => _qidErrors;
}

internal sealed class RunnerOptions
{
    public bool ShowHelp { get; private set; }

    public string LoadtestDir { get; private set; } = DefaultLoadtestDir();
    public string Host { get; private set; } = "127.0.0.1";
    public int Port { get; private set; } = 5433;
    public string Database { get; private set; } = "commerce";
    public string Username { get; private set; } = "yugabyte";
    public string? Password { get; private set; } = Environment.GetEnvironmentVariable("PGPASSWORD");

    public int RoWorkers { get; private set; } = 4;
    public int RwWorkers { get; private set; } = 8;
    public int RoOpsPerWorker { get; private set; } = 3000;
    public int RwOpsPerWorker { get; private set; } = 3000;

    public int RoMinPool { get; private set; } = 4;
    public int RoMaxPool { get; private set; } = 4;
    public bool RoMultiplexing { get; private set; } = true;

    public int RwMinPool { get; private set; } = 8;
    public int RwMaxPool { get; private set; } = 8;
    public bool RwMultiplexing { get; private set; } = false;

    public int ThinkTimeMs { get; private set; }
    public int CommandTimeoutSeconds { get; private set; } = 120;
    public int ProgressSeconds { get; private set; } = 5;
    public bool ContinueOnError { get; private set; } = true;
    public int TxBundleMaxRetries { get; private set; } = 6;
    public int TxBundleRetryBaseDelayMs { get; private set; } = 5;
    public int DurationSeconds { get; private set; }
    public double TargetTotalTps { get; private set; }
    public int ScheduleWindowSeconds { get; private set; } = 5;
    public int ScheduleQueueCapacity { get; private set; } = 20000;
    public bool IsPacedMode => DurationSeconds > 0 && TargetTotalTps > 0;

    public bool PrepareSeed { get; private set; }
    public bool AutoSeedIfEmpty { get; private set; } = true;
    public string ResetSqlFile { get; private set; } = Path.Combine(DefaultLoadtestDir(), "sql", "reset_workload_tables.sql");
    public string SeedSqlFile { get; private set; } = Path.Combine(DefaultLoadtestDir(), "sql", "seed_happy_path.sql");
    public int SeedCommandTimeoutSeconds { get; private set; } = 600;

    public static RunnerOptions Parse(string[] args)
    {
        var options = new RunnerOptions();

        for (var i = 0; i < args.Length; i++)
        {
            var arg = args[i];

            string NextValue()
            {
                if (i + 1 >= args.Length)
                {
                    throw new ArgumentException($"Missing value for {arg}");
                }

                i++;
                return args[i];
            }

            switch (arg)
            {
                case "-h":
                case "--help":
                    options.ShowHelp = true;
                    break;
                case "--loadtest-dir":
                    options.LoadtestDir = Path.GetFullPath(NextValue());
                    break;
                case "--host":
                    options.Host = NextValue();
                    break;
                case "--port":
                    options.Port = int.Parse(NextValue(), CultureInfo.InvariantCulture);
                    break;
                case "--database":
                case "--db":
                    options.Database = NextValue();
                    break;
                case "--username":
                case "--user":
                    options.Username = NextValue();
                    break;
                case "--password":
                    options.Password = NextValue();
                    break;
                case "--ro-workers":
                    options.RoWorkers = int.Parse(NextValue(), CultureInfo.InvariantCulture);
                    break;
                case "--rw-workers":
                    options.RwWorkers = int.Parse(NextValue(), CultureInfo.InvariantCulture);
                    break;
                case "--ro-ops":
                    options.RoOpsPerWorker = int.Parse(NextValue(), CultureInfo.InvariantCulture);
                    break;
                case "--rw-ops":
                    options.RwOpsPerWorker = int.Parse(NextValue(), CultureInfo.InvariantCulture);
                    break;
                case "--ro-min-pool":
                    options.RoMinPool = int.Parse(NextValue(), CultureInfo.InvariantCulture);
                    break;
                case "--ro-max-pool":
                    options.RoMaxPool = int.Parse(NextValue(), CultureInfo.InvariantCulture);
                    break;
                case "--ro-multiplexing":
                    options.RoMultiplexing = ParseBool(NextValue());
                    break;
                case "--rw-min-pool":
                    options.RwMinPool = int.Parse(NextValue(), CultureInfo.InvariantCulture);
                    break;
                case "--rw-max-pool":
                    options.RwMaxPool = int.Parse(NextValue(), CultureInfo.InvariantCulture);
                    break;
                case "--rw-multiplexing":
                    options.RwMultiplexing = ParseBool(NextValue());
                    break;
                case "--think-time-ms":
                    options.ThinkTimeMs = int.Parse(NextValue(), CultureInfo.InvariantCulture);
                    break;
                case "--command-timeout":
                    options.CommandTimeoutSeconds = int.Parse(NextValue(), CultureInfo.InvariantCulture);
                    break;
                case "--progress-seconds":
                    options.ProgressSeconds = int.Parse(NextValue(), CultureInfo.InvariantCulture);
                    break;
                case "--continue-on-error":
                    options.ContinueOnError = true;
                    break;
                case "--tx-retries":
                    options.TxBundleMaxRetries = int.Parse(NextValue(), CultureInfo.InvariantCulture);
                    break;
                case "--tx-retry-base-delay-ms":
                    options.TxBundleRetryBaseDelayMs = int.Parse(NextValue(), CultureInfo.InvariantCulture);
                    break;
                case "--duration-seconds":
                    options.DurationSeconds = int.Parse(NextValue(), CultureInfo.InvariantCulture);
                    break;
                case "--target-total-tps":
                    options.TargetTotalTps = double.Parse(NextValue(), CultureInfo.InvariantCulture);
                    break;
                case "--schedule-window-seconds":
                    options.ScheduleWindowSeconds = int.Parse(NextValue(), CultureInfo.InvariantCulture);
                    break;
                case "--schedule-queue-capacity":
                    options.ScheduleQueueCapacity = int.Parse(NextValue(), CultureInfo.InvariantCulture);
                    break;
                case "--prepare-seed":
                    options.PrepareSeed = true;
                    break;
                case "--no-auto-seed-if-empty":
                    options.AutoSeedIfEmpty = false;
                    break;
                case "--reset-sql-file":
                    options.ResetSqlFile = Path.GetFullPath(NextValue());
                    break;
                case "--seed-sql-file":
                    options.SeedSqlFile = Path.GetFullPath(NextValue());
                    break;
                case "--seed-command-timeout":
                    options.SeedCommandTimeoutSeconds = int.Parse(NextValue(), CultureInfo.InvariantCulture);
                    break;
                default:
                    throw new ArgumentException($"Unknown argument: {arg}");
            }
        }

        ApplyDerivedSettings(options);
        Validate(options);
        return options;
    }

    public static void PrintUsage()
    {
        Console.WriteLine("BomberLoadTest (Npgsql runner)");
        Console.WriteLine();
        Console.WriteLine("Usage:");
        Console.WriteLine("  dotnet run --project loadtest/npgsql-runner/BomberLoadTest.csproj -- [options]");
        Console.WriteLine();
        Console.WriteLine("Core options:");
        Console.WriteLine("  --loadtest-dir <path>      Loadtest root dir (default: ./loadtest)");
        Console.WriteLine("  --host <host>              DB host (default: 127.0.0.1)");
        Console.WriteLine("  --port <port>              DB port (default: 5433)");
        Console.WriteLine("  --db <name>                DB name (default: commerce)");
        Console.WriteLine("  --user <name>              DB user (default: yugabyte)");
        Console.WriteLine("  --password <value>         DB password (optional, or use PGPASSWORD)");
        Console.WriteLine();
        Console.WriteLine("Workload options:");
        Console.WriteLine("  --ro-workers <n>           RO workers (default: 4)");
        Console.WriteLine("  --rw-workers <n>           RW workers (default: 8)");
        Console.WriteLine("  --ro-ops <n>               Ops per RO worker (default: 3000)");
        Console.WriteLine("  --rw-ops <n>               Ops per RW worker (default: 3000)");
        Console.WriteLine("  --ro-min-pool <n>          RO pool min (ignored; auto=min=max ro_workers+rw_workers)");
        Console.WriteLine("  --ro-max-pool <n>          RO pool max (ignored; auto=min=max ro_workers+rw_workers)");
        Console.WriteLine("  --ro-multiplexing <bool>   RO multiplexing (default: true)");
        Console.WriteLine("  --rw-min-pool <n>          RW pool min (ignored; auto=min=max ro_workers+rw_workers)");
        Console.WriteLine("  --rw-max-pool <n>          RW pool max (ignored; auto=min=max ro_workers+rw_workers)");
        Console.WriteLine("  --rw-multiplexing <bool>   RW multiplexing (default: false)");
        Console.WriteLine("  --think-time-ms <n>        Delay between ops per worker (default: 0)");
        Console.WriteLine("  --command-timeout <sec>    Command timeout seconds (default: 120)");
        Console.WriteLine("  --progress-seconds <sec>   Progress print interval (default: 5, 0 disables)");
        Console.WriteLine("  --tx-retries <n>           Max retries for TXBUNDLE on serialization/deadlock (default: 6)");
        Console.WriteLine("  --tx-retry-base-delay-ms <n> Base backoff delay for TXBUNDLE retries (default: 5)");
        Console.WriteLine("  --duration-seconds <n>     Paced mode duration in seconds (default: disabled)");
        Console.WriteLine("  --target-total-tps <n>     Paced mode total target TPS across RO+RW (default: disabled)");
        Console.WriteLine("  --schedule-window-seconds <n> Scheduler window size for per-QID proportional batches (default: 5)");
        Console.WriteLine("  --schedule-queue-capacity <n> Bounded channel capacity for paced scheduling (default: 20000)");
        Console.WriteLine("  --continue-on-error        Kept for compatibility (always enabled in HA mode)");
        Console.WriteLine();
        Console.WriteLine("Seed/reset options:");
        Console.WriteLine("  --prepare-seed             Run reset+seed before load starts");
        Console.WriteLine("  --no-auto-seed-if-empty    Do not auto-run seed when required key pools are empty");
        Console.WriteLine("  --reset-sql-file <path>    SQL file for workload reset (default: loadtest/sql/reset_workload_tables.sql)");
        Console.WriteLine("  --seed-sql-file <path>     SQL file for seed (default: loadtest/sql/seed_happy_path.sql)");
        Console.WriteLine("  --seed-command-timeout <s> Timeout for reset/seed SQL files (default: 600)");
    }

    private static string DefaultLoadtestDir()
    {
        return Path.GetFullPath(Path.Combine(AppContext.BaseDirectory, "..", "..", "..", ".."));
    }

    private static bool ParseBool(string value)
    {
        if (bool.TryParse(value, out var parsed))
        {
            return parsed;
        }

        throw new ArgumentException($"Invalid boolean value: {value}");
    }

    private static void ApplyDerivedSettings(RunnerOptions options)
    {
        var totalWorkers = options.RoWorkers + options.RwWorkers;
        options.RoMinPool = totalWorkers;
        options.RoMaxPool = totalWorkers;
        options.RwMinPool = totalWorkers;
        options.RwMaxPool = totalWorkers;
        options.ContinueOnError = true;
    }

    private static void Validate(RunnerOptions options)
    {
        if (options.RoWorkers <= 0 || options.RwWorkers <= 0)
        {
            throw new ArgumentException("Worker counts must be > 0.");
        }

        if (options.RoOpsPerWorker <= 0 || options.RwOpsPerWorker <= 0)
        {
            throw new ArgumentException("Ops per worker must be > 0.");
        }

        if (options.RoMinPool <= 0 || options.RoMaxPool <= 0 || options.RwMinPool <= 0 || options.RwMaxPool <= 0)
        {
            throw new ArgumentException("Pool sizes must be > 0.");
        }

        if (options.RoMinPool > options.RoMaxPool || options.RwMinPool > options.RwMaxPool)
        {
            throw new ArgumentException("Pool min cannot be greater than pool max.");
        }

        if (options.CommandTimeoutSeconds <= 0)
        {
            throw new ArgumentException("Command timeout must be > 0.");
        }

        if (options.ThinkTimeMs < 0)
        {
            throw new ArgumentException("Think time must be >= 0.");
        }

        if (options.ProgressSeconds < 0)
        {
            throw new ArgumentException("Progress seconds must be >= 0.");
        }

        if (options.TxBundleMaxRetries < 0)
        {
            throw new ArgumentException("TX retries must be >= 0.");
        }

        if (options.TxBundleRetryBaseDelayMs < 0)
        {
            throw new ArgumentException("TX retry base delay must be >= 0.");
        }

        if (options.DurationSeconds < 0)
        {
            throw new ArgumentException("Duration seconds must be >= 0.");
        }

        if (options.TargetTotalTps < 0)
        {
            throw new ArgumentException("Target total TPS must be >= 0.");
        }

        if ((options.DurationSeconds > 0) != (options.TargetTotalTps > 0))
        {
            throw new ArgumentException("Paced mode requires both --duration-seconds and --target-total-tps.");
        }

        if (options.ScheduleWindowSeconds <= 0)
        {
            throw new ArgumentException("Schedule window seconds must be > 0.");
        }

        if (options.ScheduleQueueCapacity <= 0)
        {
            throw new ArgumentException("Schedule queue capacity must be > 0.");
        }

        if (options.SeedCommandTimeoutSeconds <= 0)
        {
            throw new ArgumentException("Seed command timeout must be > 0.");
        }

        if (string.IsNullOrWhiteSpace(options.ResetSqlFile))
        {
            throw new ArgumentException("Reset SQL file cannot be empty.");
        }

        if (string.IsNullOrWhiteSpace(options.SeedSqlFile))
        {
            throw new ArgumentException("Seed SQL file cannot be empty.");
        }
    }
}
