// One workload per amp endpoint: transactions, a conserved bank total, TPC-C.
package stress

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps"
	"github.com/yugabyte/amp/test/functional/framework/apps/benchbase"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/sampleapp"
	"github.com/yugabyte/amp/test/functional/framework/exec"
)

// Markers meaning the workload found something WRONG, not a retryable reset.
var workloadViolationMarkers = []string{
	"Failed to compare sums",
	"Forbidden exception detected",
}

type endpointWorkload struct {
	Name    string
	Tenant  int
	start   func(context.Context) error
	alive   func(context.Context) (bool, error)
	logTail func(context.Context) string
	collect func(context.Context, apps.ArtifactSink) error
	// exitIsFinding: sample-apps reconnect, so dead means real failure; false for TPC-C unless it's the reconnecting fork.
	exitIsFinding bool

	stopped bool
	exited  bool
}

type workloadSet struct {
	workloads []*endpointWorkload
	logf      func(string, ...any)
	mu        sync.Mutex
}

// Start stops at the first workload that fails to avoid vacuous later checks.
func (w *workloadSet) Start(ctx context.Context) error {
	for _, wl := range w.workloads {
		if err := wl.start(ctx); err != nil {
			return fmt.Errorf("%s (tenant %d) never started generating load: %w", wl.Name, wl.Tenant, err)
		}
		w.logf("workload %s is driving tenant %d's endpoint", wl.Name, wl.Tenant)
	}
	return nil
}

func (w *workloadSet) Check(ctx context.Context, after string) error {
	var problems []string
	for _, wl := range w.workloads {
		alive, err := wl.alive(ctx)
		if err != nil {
			w.logf("workload %s: could not read liveness after %s: %v", wl.Name, after, err)
			continue
		}
		tail := wl.logTail(ctx)
		// Fatal however the workload ended — checked before liveness.
		if v := violations(tail); v != "" {
			problems = append(problems, fmt.Sprintf("%s (tenant %d) reported %s", wl.Name, wl.Tenant, v))
			continue
		}
		if alive {
			continue
		}
		if !wl.exitIsFinding {
			if !wl.exited {
				wl.exited = true
				w.logf("workload %s stopped during %s with no consistency violation — it has no connection-death retry, which a rolling operation triggers; tolerated", wl.Name, after)
			}
			continue
		}
		problems = append(problems, fmt.Sprintf("%s (tenant %d) is no longer running%s",
			wl.Name, wl.Tenant, violationDetail(tail)))
	}
	if len(problems) > 0 {
		return fmt.Errorf("after %s: %s", after, strings.Join(problems, "; "))
	}
	return nil
}

func (w *workloadSet) CollectAll(sink apps.ArtifactSink) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, wl := range w.workloads {
		if wl.stopped || wl.collect == nil {
			continue
		}
		wl.stopped = true
		ctx, cancel := context.WithTimeout(context.Background(), reportKillTimeout)
		if err := wl.collect(ctx, sink); err != nil {
			w.logf("collect %s (best-effort): %v", wl.Name, err)
		}
		cancel()
	}
}

func violations(tail string) string {
	var found []string
	for _, m := range workloadViolationMarkers {
		if strings.Contains(tail, m) {
			found = append(found, fmt.Sprintf("%q", m))
		}
	}
	if len(found) == 0 {
		return ""
	}
	return "a consistency violation in its log (" + strings.Join(found, ", ") + ")"
}

func violationDetail(tail string) string {
	if v := violations(tail); v != "" {
		return " — " + v
	}
	return ""
}

// Password is required: a pod dialling the compute Service needs it, unlike the coordinator's port-forward.
func endpointProps(user, password, database string) map[string]string {
	return map[string]string{
		"username":                  user,
		"password":                  password,
		"default_postgres_database": database,
	}
}

// tablets=0 omits the jar's YugabyteDB-only "SPLIT INTO n TABLETS" DDL.
func bankOnAmpProps(accounts int) map[string]string {
	return map[string]string{
		"bank_exit_on_fail":       "true",
		"bank_number_of_tablets":  "0",
		"bank_number_of_accounts": strconv.Itoa(accounts),
	}
}

func sampleAppOnEndpoint(ex exec.RemoteExecutor, jarPath string, wl sampleapp.Workload,
	tenantIndex int, host string, port int, creds endpointCreds, cfg maintenanceConfig,
	marker, uuid string, extra map[string]string, logf func(string, ...any)) *endpointWorkload {

	props := endpointProps(creds.User, creds.Password, creds.Database)
	for k, v := range extra {
		props[k] = v
	}
	app := sampleapp.New(ex, sampleapp.RunSpec{
		JarPath:         jarPath,
		Workload:        wl,
		Hosts:           []string{host},
		YSQLPort:        port,
		NumThreadsRead:  cfg.WorkloadThreadsRead,
		NumThreadsWrite: cfg.WorkloadThreadsWrite,
		RunTime:         cfg.WorkloadRunTime,
		Marker:          marker,
		UUID:            uuid,
		Props:           props,
	}, sampleapp.WithLogf(logf), sampleapp.WithReadyTimeout(cfg.SettleTimeout))

	return &endpointWorkload{
		Name:          string(wl),
		Tenant:        tenantIndex,
		exitIsFinding: true,
		start: func(ctx context.Context) error {
			if err := app.Start(ctx); err != nil {
				return err
			}
			return app.WaitReady(ctx)
		},
		alive:   func(ctx context.Context) (bool, error) { return app.Running(ctx) },
		logTail: func(ctx context.Context) string { return app.LogTail(ctx) },
		collect: func(ctx context.Context, sink apps.ArtifactSink) error { return app.ReportKill(ctx, sink) },
	}
}

// BenchBase's create+load+execute is one blocking call, so it runs in the background.
func benchbaseOnEndpoint(ex exec.RemoteExecutor, runDir string, tenantIndex int,
	host string, port int, creds endpointCreds, cfg maintenanceConfig, logf func(string, ...any)) *endpointWorkload {

	spec := benchbase.RunSpec{
		RunDir:     runDir,
		Benchmark:  benchbase.TPCC,
		Conn:       pg.ConnInfo{Host: host, Port: port, User: creds.User, Password: creds.Password, Database: creds.Database, SSLMode: "disable"},
		Warehouses: cfg.BenchWarehouses,
		Terminals:  cfg.BenchTerminals,
		Duration:   cfg.WorkloadRunTime,
		// Per-tenant: concurrent tpcc tenants share runDir, and the default
		// benchbase.out (and its .exit twin) would collide.
		LogPath: fmt.Sprintf("benchbase-t%d.out", tenantIndex),
	}
	var mu sync.Mutex
	var raw string
	var runErr error
	done := make(chan struct{})

	return &endpointWorkload{
		Name:   "tpcc",
		Tenant: tenantIndex,
		start: func(ctx context.Context) error {
			go func() {
				defer close(done)
				// Detached: the run outlives start and stops on its own duration.
				res, err := benchbase.Run(context.WithoutCancel(ctx), ex, spec)
				mu.Lock()
				raw, runErr = res.Raw, err
				mu.Unlock()
			}()
			// No "generating load" signal; the grace period catches a bad connection.
			select {
			case <-done:
				mu.Lock()
				defer mu.Unlock()
				if runErr != nil {
					return runErr
				}
				return fmt.Errorf("tpcc finished before the maintenance even started")
			case <-time.After(cfg.BenchStartGrace):
				return nil
			}
		},
		alive: func(context.Context) (bool, error) {
			select {
			case <-done:
				return false, nil
			default:
				return true, nil
			}
		},
		// The log file, not Run's return value, which arrives only once TPC-C exits.
		logTail: func(ctx context.Context) string {
			return benchbaseLog(ctx, ex, spec)
		},
		collect: func(ctx context.Context, sink apps.ArtifactSink) error {
			out := benchbaseLog(ctx, ex, spec)
			if out == "" {
				mu.Lock()
				out = raw
				mu.Unlock()
			}
			if out == "" {
				return nil
			}
			return sink.Add("benchbase-tpcc.out", "log", []byte(out))
		},
	}
}

type endpointCreds struct {
	User     string
	Password string
	Database string
}

// benchbaseLog is best-effort: the file does not exist until the run writes.
func benchbaseLog(ctx context.Context, ex exec.RemoteExecutor, spec benchbase.RunSpec) string {
	res, err := ex.Run(ctx, "cat "+spec.ResolvedLogPath()+" 2>/dev/null")
	if err != nil {
		return ""
	}
	return res.Stdout
}
