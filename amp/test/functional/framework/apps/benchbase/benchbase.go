// Package benchbase drives the BenchBase benchmark suite (the yugabyte fork)
// REMOTELY over an exec.RemoteExecutor — the client-pod rule: the benchmark
// never runs on the coordinator. It is the ampqa analog of the stress
// framework's src/apps/benchbase/benchbase_app.py.
//
// A BenchBase run is `java -jar benchbase.jar -b <benchmark> -c <config.xml>
// --create --load --execute` inside the unpacked benchbase-yugabyte/ directory.
// The connection + load parameters live in the benchmark's XML config; this app
// patches them in place (target address, db/user/password, scalefactor,
// terminals, time, rate) exactly as the stress driver does with sed, then runs
// the jar to completion and parses the summary it writes under results/.
//
// TPC-C is the default, with deliberately small load params (a couple of
// warehouses/terminals, a short duration) so a real run stays cheap.
package benchbase

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/exec"
)

// Benchmark is the value passed to the jar's -b flag.
type Benchmark string

const (
	TPCC        Benchmark = "tpcc"
	TPCH        Benchmark = "tpch"
	YCSB        Benchmark = "ycsb"
	SmallBank   Benchmark = "smallbank"
	Noop        Benchmark = "noop"
	CHBenchmark Benchmark = "chbenchmark"
)

// Defaults (mirrored in configs/default.yaml under apps.benchbase.*).
const (
	DefaultBenchmark  = "tpcc"
	DefaultWarehouses = 1
	DefaultTerminals  = 1
	DefaultDuration   = 60 * time.Second
	DefaultRate       = "unlimited"
	DefaultMaxHeap    = "8g" // java -Xmx
	DefaultSubdir     = "benchbase-yugabyte"
	// -im: how often BenchBase writes a results row. This IS the resolution of
	// every benchbase-driven time series (tpcc, ycsb): at 5000 a 3-minute window
	// charted 36 points, which hides anything shorter than a five-second dip.
	// 1s matches what the pgbench driver already emits via -P, so all five
	// multitenant workloads now share one x-resolution and can be read together.
	intervalMonitorMS = 1000
)

// missingJarHint names the artifact route to fix a missing benchbase.jar,
// returned verbatim so callers/logs point at the fix.
const missingJarHint = "benchbase.jar not found on target; deploy it via the benchbase artifact " +
	"(artifact.Deploy with artifacts[\"benchbase\"], remote_unzip) before Run"

// RunSpec is one BenchBase run.
type RunSpec struct {
	// RunDir is the unpacked benchbase-yugabyte directory on the target (contains
	// benchbase.jar and config/). Empty → uses the current directory.
	RunDir    string
	Benchmark Benchmark
	// ConfigPath is the benchmark XML config, relative to RunDir. Empty →
	// config/yugabyte/sample_<benchmark>_config.xml (the fork's sample configs).
	ConfigPath string

	Conn pg.ConnInfo

	// Addrs is the FULL list of target "host:port"s to write into the JDBC URL,
	// which the driver accepts in pgjdbc's multi-host form
	// ("jdbc:yugabytedb://h1:p,h2:p,h3:p/db"). Empty → Conn's single address, the
	// exact URL shape a single-target run has always produced. More than one entry
	// additionally turns loadBalanceHosts on (see BuildSeds).
	Addrs []string

	// Scalefactor is the benchmark's scale. For TPC-C it is the WAREHOUSE count;
	// 0 → Warehouses (below), then DefaultWarehouses.
	Scalefactor int
	Warehouses  int
	Terminals   int
	Duration    time.Duration // → <time> in whole seconds
	Rate        string        // → <rate> ("unlimited" or a number); empty → DefaultRate

	// Phases; all default true (a full create+load+execute cycle).
	Create  *bool
	Load    *bool
	Execute *bool

	MaxHeap string // java -Xmx (e.g. "8g"); empty → DefaultMaxHeap
	LogPath string // stdout+stderr capture; empty → benchbase.out under RunDir

	PollInterval time.Duration // detached-run poll cadence; empty → 5s

	// LoadBalance, when non-nil, forces the YugabyteDB smart-driver URL's
	// load-balance flag on/off (the fork's sample configs ship load-balance=true).
	// Set it false for an in-cluster (k8s) target reached through a Service: the
	// smart driver's topology discovery (yb_servers()) returns tserver node
	// addresses that a client pod cannot route to, so a load-balancing connect
	// fails where a direct connect through the Service succeeds. nil leaves the
	// config as-is (the VM default, where discovered addresses ARE reachable).
	LoadBalance *bool
}

func (s RunSpec) benchmark() Benchmark {
	if s.Benchmark != "" {
		return s.Benchmark
	}
	return DefaultBenchmark
}

func (s RunSpec) configPath() string {
	if s.ConfigPath != "" {
		return s.ConfigPath
	}
	return fmt.Sprintf("config/yugabyte/sample_%s_config.xml", s.benchmark())
}

// urlTarget is what replaces the sample config's "localhost:5433" in the JDBC
// URL: every address in Addrs (comma-joined, the pgjdbc multi-host authority),
// else Conn's single address.
func (s RunSpec) urlTarget() string {
	if len(s.Addrs) > 0 {
		return strings.Join(s.Addrs, ",")
	}
	return s.Conn.Addr()
}

func (s RunSpec) scalefactor() int {
	if s.Scalefactor > 0 {
		return s.Scalefactor
	}
	if s.Warehouses > 0 {
		return s.Warehouses
	}
	return DefaultWarehouses
}

func (s RunSpec) terminals() int {
	if s.Terminals > 0 {
		return s.Terminals
	}
	return DefaultTerminals
}

func (s RunSpec) duration() time.Duration {
	if s.Duration > 0 {
		return s.Duration
	}
	return DefaultDuration
}

func (s RunSpec) rate() string {
	if s.Rate != "" {
		return s.Rate
	}
	return DefaultRate
}

func (s RunSpec) maxHeap() string {
	if s.MaxHeap != "" {
		return s.MaxHeap
	}
	return DefaultMaxHeap
}

func (s RunSpec) logPath() string {
	if s.LogPath != "" {
		return s.LogPath
	}
	return "benchbase.out"
}

// ResolvedLogPath is where the run writes its output ON THE TARGET, run dir
// included. The command redirects into it, so a caller can read the log WHILE
// the benchmark is still going — Run only surfaces the output once it returns,
// which for a long benchmark is far too late to notice anything.
func (s RunSpec) ResolvedLogPath() string {
	if s.RunDir == "" {
		return s.logPath()
	}
	return path.Join(s.RunDir, s.logPath())
}

func (s RunSpec) pollInterval() time.Duration {
	if s.PollInterval > 0 {
		return s.PollInterval
	}
	return 5 * time.Second
}

func phase(p *bool) bool { return p == nil || *p }

// Ptr is a helper for setting the optional Create/Load/Execute phase flags.
func Ptr(b bool) *bool { return &b }

// Result is the parsed outcome of a BenchBase run. Raw always carries the full
// captured stdout+summary, even when parsing found nothing, so a caller can
// attach it for triage regardless.
type Result struct {
	ThroughputReqPerSec float64
	GoodputReqPerSec    float64
	MedianLatencyUS     float64
	P95LatencyUS        float64
	P99LatencyUS        float64
	Raw                 string
}

// BuildSeds renders the in-place `sed` edits that patch the benchmark XML with
// the run's connection + load parameters, mirroring the stress driver. Pure and
// exported for test assertions.
func BuildSeds(spec RunSpec) []string {
	cfg := shellQuote(spec.configPath())
	var seds []string
	add := func(expr string) {
		seds = append(seds, fmt.Sprintf("sed -i -e %s %s", shellQuote(expr), cfg))
	}
	// Target address: the fork's sample configs point at localhost:5433. The
	// interpolated values land in sed REPLACEMENT position, so their sed
	// metacharacters (delimiter |, & = matched text, \ = escape) must be escaped
	// or a value like a password containing one corrupts the whole expression.
	add(fmt.Sprintf("s|localhost:5433|%s|g", sedReplace(spec.urlTarget())))
	// DB name: the sample url is …/yugabyte?sslmode=…; swap the db, keep the query.
	if db := spec.Conn.Database; db != "" && db != "yugabyte" {
		add(fmt.Sprintf("s|yugabyte?sslmode|%s?sslmode|g", sedReplace(db)))
	}
	if u := spec.Conn.User; u != "" {
		add(fmt.Sprintf("s|<username>.*</username>|<username>%s</username>|", sedReplace(u)))
	}
	if p := spec.Conn.Password; p != "" {
		add(fmt.Sprintf("s|<password>.*</password>|<password>%s</password>|", sedReplace(p)))
	}
	add(fmt.Sprintf("s|<scalefactor>.*</scalefactor>|<scalefactor>%d</scalefactor>|", spec.scalefactor()))
	add(fmt.Sprintf("s|<terminals>.*</terminals>|<terminals>%d</terminals>|", spec.terminals()))
	add(fmt.Sprintf("s|<time>.*</time>|<time>%d</time>|", int(spec.duration().Round(time.Second)/time.Second)))
	add(fmt.Sprintf("s|<rate>.*</rate>|<rate>%s</rate>|", spec.rate()))
	if spec.LoadBalance != nil {
		repl := fmt.Sprintf("load-balance=%t", *spec.LoadBalance)
		// Several hosts in the URL with the smart driver's own cluster-aware
		// balancing off: switch on pgjdbc's loadBalanceHosts, which shuffles the
		// URL's host list per connection (MultiHostChooser) — so connections spread
		// over exactly the hosts we named, with no yb_servers() topology discovery of
		// addresses the client may not be able to route to. `&` is the sed
		// matched-text metachar, and the XML escapes the query separator, hence
		// the literal `\&amp;`.
		if !*spec.LoadBalance && len(spec.Addrs) > 1 {
			repl += `\&amp;loadBalanceHosts=true`
		}
		add(fmt.Sprintf("s|load-balance=true|%s|g", repl))
	}
	return seds
}

// BuildRunCommand renders the full `cd … && sed … && java -jar benchbase.jar …`
// invocation. Pure and exported so tests assert the exact command.
func BuildRunCommand(spec RunSpec) string {
	var b strings.Builder
	if spec.RunDir != "" {
		fmt.Fprintf(&b, "cd %s && ", shellQuote(spec.RunDir))
	}
	for _, s := range BuildSeds(spec) {
		b.WriteString(s)
		b.WriteString(" && ")
	}
	fmt.Fprintf(&b, "java -Xmx%s -jar benchbase.jar -b %s -c %s",
		spec.maxHeap(), shellQuote(string(spec.benchmark())), shellQuote(spec.configPath()))
	fmt.Fprintf(&b, " --create=%s --load=%s --execute=%s",
		strconv.FormatBool(phase(spec.Create)), strconv.FormatBool(phase(spec.Load)), strconv.FormatBool(phase(spec.Execute)))
	fmt.Fprintf(&b, " -im %d", intervalMonitorMS)
	fmt.Fprintf(&b, " > %s 2>&1", shellQuote(spec.logPath()))
	return b.String()
}

// ExitPath is where the detached wrapper records the benchmark's exit code,
// next to the log on the target.
func (s RunSpec) ExitPath() string { return s.ResolvedLogPath() + ".exit" }

// BuildStartCommand renders the detached launch: the whole run (sed patches,
// java, log redirect) goes into a nohup'd shell that records its exit code,
// and only the wrapper's pid comes back. A benchmark held on a live exec
// stream dies with that stream (kubectl exec through the API server drops on
// multi-minute sessions); a detached one survives it.
func BuildStartCommand(spec RunSpec) string {
	inner := BuildRunCommand(spec) + "; echo $? > " + shellQuote(spec.ExitPath())
	// ";" not "&&": with "rm && nohup … & echo $!" the "&" grabs the whole list,
	// so $! would be a wrapping subshell and rm would race the first poll.
	return "rm -f " + shellQuote(spec.ExitPath()) +
		"; nohup sh -c " + shellQuote(inner) + " >/dev/null 2>&1 & echo $!"
}

// A dropped poll must not kill the run, but a target that stopped answering
// entirely has to surface eventually.
const maxConsecutivePollErrors = 24

// Run patches the config, runs the benchmark to completion on ex, and parses
// the summary. It checks the jar is present first, returning missingJarHint
// (naming the artifact route) rather than a bare exec failure. The benchmark
// runs detached on the target and is watched by short polls, so a dropped
// exec stream costs one poll, never the run. Result.Raw always carries the
// captured output, even on failure.
func Run(ctx context.Context, ex exec.RemoteExecutor, spec RunSpec) (Result, error) {
	if err := checkAvailable(ctx, ex, spec); err != nil {
		return Result{}, err
	}
	res, err := ex.Run(ctx, BuildStartCommand(spec))
	if err != nil {
		return Result{}, fmt.Errorf("benchbase start: %w", err)
	}
	pid := strings.TrimSpace(res.Stdout)
	if res.ExitCode != 0 || pid == "" {
		return Result{}, fmt.Errorf("benchbase start exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stdout+res.Stderr))
	}
	exitCode, err := waitDetached(ctx, ex, spec, pid)
	if err != nil {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		raw := readLog(rctx, ex, spec)
		cancel()
		return Result{Raw: raw}, err
	}
	if exitCode != 0 {
		logTxt := readLog(ctx, ex, spec)
		raw := "--- " + spec.logPath() + " ---\n" + logTxt
		detail := tailLines(logTxt, 3)
		if c := causeLine(logTxt); c != "" {
			detail = c
		}
		if detail == "" {
			detail = "no output in " + spec.ResolvedLogPath() + " (setup failed before java, or the log is unreadable)"
		}
		return Result{Raw: raw}, fmt.Errorf("benchbase run exited %d: %s", exitCode, detail)
	}
	// The authoritative numbers are in the summary JSON BenchBase writes under
	// results/; fall back to the log when it can't be read.
	summary := readSummary(ctx, ex, spec)
	raw := summary
	if raw == "" {
		raw = readLog(ctx, ex, spec)
	}
	return parseSummary(raw), nil
}

// waitDetached polls the exit file / wrapper pid until the run ends. Poll
// failures are tolerated up to maxConsecutivePollErrors; ctx cancellation
// kills the wrapper best-effort.
func waitDetached(ctx context.Context, ex exec.RemoteExecutor, spec RunSpec, pid string) (int, error) {
	probe := "cat " + shellQuote(spec.ExitPath()) + " 2>/dev/null" +
		" || (kill -0 " + pid + " 2>/dev/null && echo RUNNING || echo GONE)"
	pollErrs := 0
	gone := false
	for {
		select {
		case <-ctx.Done():
			// Children first: killing the wrapper reparents the JVM and orphans it.
			kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			_, _ = ex.Run(kctx, "pkill -9 -P "+pid+" 2>/dev/null; kill -9 "+pid+" 2>/dev/null")
			cancel()
			return 0, fmt.Errorf("benchbase run abandoned: %w", ctx.Err())
		case <-time.After(spec.pollInterval()):
		}
		res, err := ex.Run(ctx, probe)
		if err != nil || res.ExitCode != 0 {
			pollErrs++
			if pollErrs >= maxConsecutivePollErrors {
				return 0, fmt.Errorf("benchbase run unwatchable, %d consecutive poll failures, last: %v (%s)", pollErrs, err, strings.TrimSpace(res.Stderr))
			}
			continue
		}
		pollErrs = 0
		switch out := strings.TrimSpace(res.Stdout); out {
		case "RUNNING":
			gone = false
		case "GONE":
			// One grace poll: the exit-file write may have raced the pid check.
			if gone {
				return 0, fmt.Errorf("benchbase process (pid %s) disappeared without writing %s, killed externally", pid, spec.ExitPath())
			}
			gone = true
		default:
			code, cerr := strconv.Atoi(out)
			if cerr != nil {
				return 0, fmt.Errorf("benchbase exit file %s holds %q, not an exit code", spec.ExitPath(), out)
			}
			return code, nil
		}
	}
}

// checkAvailable detects a missing benchbase.jar on the target.
func checkAvailable(ctx context.Context, ex exec.RemoteExecutor, spec RunSpec) error {
	jar := "benchbase.jar"
	if spec.RunDir != "" {
		jar = path.Join(spec.RunDir, "benchbase.jar")
	}
	res, err := ex.Run(ctx, fmt.Sprintf("test -f %s", shellQuote(jar)))
	if err != nil {
		return fmt.Errorf("benchbase: probe jar: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s", missingJarHint)
	}
	return nil
}

// readSummary cats the newest results/*.summary.json BenchBase wrote (best
// effort — an empty string just means the parser falls back to stdout).
func readSummary(ctx context.Context, ex exec.RemoteExecutor, spec RunSpec) string {
	dir := "results"
	if spec.RunDir != "" {
		dir = path.Join(spec.RunDir, "results")
	}
	cmd := fmt.Sprintf("cat $(ls -t %s/*.summary.json 2>/dev/null | head -n1) 2>/dev/null", shellQuote(dir))
	res, err := ex.Run(ctx, cmd)
	if err != nil {
		return ""
	}
	return res.Stdout
}

// readLog cats the benchmark's redirected stdout/stderr log (spec.logPath()
// under RunDir), best effort — used to surface the real failure when the run
// exits non-zero.
func readLog(ctx context.Context, ex exec.RemoteExecutor, spec RunSpec) string {
	log := spec.logPath()
	if spec.RunDir != "" {
		log = path.Join(spec.RunDir, log)
	}
	res, err := ex.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null", shellQuote(log)))
	if err != nil {
		return ""
	}
	return res.Stdout
}

// causeLine returns the first log line that names the underlying failure — a
// thrown exception or a connection error — so the error message leads with the
// real cause rather than a stack-frame tail. Empty if none matches.
func causeLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "at ") {
			continue
		}
		if strings.Contains(t, "Exception") || strings.Contains(t, "Caused by") ||
			strings.Contains(t, "Connection to") || strings.Contains(t, "Error") {
			return t
		}
	}
	return ""
}

// tailLines returns the last n non-empty lines of s, joined by "; " — a compact
// failure detail for an error message.
func tailLines(s string, n int) string {
	var lines []string
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			lines = append(lines, t)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "; ")
}

// summaryDoc is the subset of BenchBase's summary JSON we read. Keys match the
// fork's ResultWriter output; missing keys leave the field zero.
type summaryDoc struct {
	Throughput float64 `json:"Throughput (requests/second)"`
	Goodput    float64 `json:"Goodput (requests/second)"`
	Latency    struct {
		Median float64 `json:"Median Latency (microseconds)"`
		P95    float64 `json:"95th Percentile Latency (microseconds)"`
		P99    float64 `json:"99th Percentile Latency (microseconds)"`
	} `json:"Latency Distribution"`
}

// parseSummary extracts throughput/latency from the summary JSON embedded in
// raw. Missing/unparseable data leaves fields zero; Raw always carries the full
// text for a caller/attachment to fall back on.
func parseSummary(raw string) Result {
	r := Result{Raw: raw}
	if start := strings.Index(raw, "{"); start >= 0 {
		if end := strings.LastIndex(raw, "}"); end > start {
			var doc summaryDoc
			if err := json.Unmarshal([]byte(raw[start:end+1]), &doc); err == nil {
				r.ThroughputReqPerSec = doc.Throughput
				r.GoodputReqPerSec = doc.Goodput
				r.MedianLatencyUS = doc.Latency.Median
				r.P95LatencyUS = doc.Latency.P95
				r.P99LatencyUS = doc.Latency.P99
			}
		}
	}
	return r
}

// --- config ---

// Getter is the subset of core.Config the FromConfig reader needs.
type Getter interface {
	GetString(key, def string) string
	GetInt(key string, def int) int
	GetDuration(key string, def time.Duration) time.Duration
}

// Config is the resolved apps.benchbase.* configuration.
type Config struct {
	Benchmark   Benchmark
	Warehouses  int
	Terminals   int
	Duration    time.Duration
	Scalefactor int
	Rate        string
}

// FromConfig reads apps.benchbase.* into a Config, applying the canonical
// defaults.
func FromConfig(get Getter) Config {
	return Config{
		Benchmark:   Benchmark(get.GetString("apps.benchbase.benchmark", DefaultBenchmark)),
		Warehouses:  get.GetInt("apps.benchbase.warehouses", DefaultWarehouses),
		Terminals:   get.GetInt("apps.benchbase.terminals", DefaultTerminals),
		Duration:    get.GetDuration("apps.benchbase.duration", DefaultDuration),
		Scalefactor: get.GetInt("apps.benchbase.scalefactor", 0),
		Rate:        get.GetString("apps.benchbase.rate", DefaultRate),
	}
}

// ApplyTo overlays cfg onto spec, filling the benchmark/load fields from config
// while leaving the caller-supplied RunDir/Conn untouched.
func (cfg Config) ApplyTo(spec RunSpec) RunSpec {
	spec.Benchmark = cfg.Benchmark
	spec.Warehouses = cfg.Warehouses
	spec.Terminals = cfg.Terminals
	spec.Duration = cfg.Duration
	spec.Scalefactor = cfg.Scalefactor
	spec.Rate = cfg.Rate
	return spec
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sedReplace escapes the sed metacharacters that are special in the REPLACEMENT
// half of an `s|…|repl|` expression: backslash (escape), ampersand (expands to
// the matched text), and the `|` delimiter. Backslash first so the escapes it
// introduces are not themselves re-escaped.
func sedReplace(s string) string {
	return strings.NewReplacer(`\`, `\\`, `&`, `\&`, `|`, `\|`).Replace(s)
}
