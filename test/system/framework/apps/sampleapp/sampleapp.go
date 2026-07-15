// Package sampleapp drives the YugabyteDB Java sample-apps jar (the
// yb-stress-sample-apps fork) REMOTELY over an exec.RemoteExecutor — the
// client-pod rule: the workload never runs on the coordinator. It is the ampqa
// analog of the stress framework's src/apps/sample/sample_app_main.py.
//
// The jar is a background `nohup java -jar … --workload <W> --nodes h:p &`
// process against the backing YugabyteDB (YSQL for Sql* workloads, YCQL for
// Cassandra* workloads). Lifecycle mirrors the stress driver: Start (deploy the
// jar when configured, then launch) → WaitReady (wait for load to be going) →
// Running (monitor) → Collect (pull the workload log) → ReportKill (collect the
// log BEFORE killing, apps.StopWithCollect — losing workload evidence to a kill
// is the report_kill lesson AGENTS.md §5 encodes).
//
// The two-phase SqlDataLoad pattern (create schema only with SkipWorkload, then
// load data into it with SkipDDL) is supported by the SkipWorkload/SkipDDL
// flags on RunSpec — a caller runs the app twice, once per phase.
package sampleapp

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/yugabyte/yb-amp/test/system/framework/apps"
	"github.com/yugabyte/yb-amp/test/system/framework/artifact"
	"github.com/yugabyte/yb-amp/test/system/framework/exec"
)

// Workload is the value passed to the jar's --workload flag. The jar exposes
// many more (every SampleAppWorkloads entry in the stress driver); these are the
// common ones surfaced as constants — any other name works as a bare Workload
// string too.
type Workload string

const (
	SqlInserts        Workload = "SqlInserts"
	SqlDataLoad       Workload = "SqlDataLoad"
	SqlBankTransfer   Workload = "SqlBankTransfer"
	SqlTransactions   Workload = "SqlTransactions"
	CassandraKeyValue Workload = "CassandraKeyValue"
)

// isYCQL reports whether the workload targets YCQL (Cassandra* families) rather
// than YSQL, selecting which port --nodes advertises — the same split the
// stress driver makes.
func (w Workload) isYCQL() bool { return strings.HasPrefix(string(w), "Cassandra") }

// Defaults (mirrored in configs/default.yaml under apps.sampleapp.*).
const (
	DefaultYSQLPort     = 5433
	DefaultYCQLPort     = 9042
	DefaultWorkload     = "SqlInserts"
	DefaultNumTables    = 1
	DefaultReadyTimeout = 5 * time.Minute

	// markerProp is the JVM system property carrying the per-run marker; it lets
	// pgrep/pkill target exactly this launch (and nothing else) without a fragile
	// match on the shared `java -jar …` command line.
	markerProp = "ampqa.sampleapp.marker"
)

// missingJarHint names the artifact route to fix a missing jar, returned
// verbatim so callers/logs point at the fix instead of a bare exec error.
const missingJarHint = "sample-app jar not found on target; deploy it via the sampleapp artifact " +
	"(sampleapp.WithDeploy / artifact.Deploy with artifacts[\"sampleapp\"]) before Start"

// RunSpec is one sample-app launch: the jar to run, the workload, the backing
// endpoints, and the per-workload knobs. Zero-valued numeric knobs are omitted
// from the command (the jar applies its own default); the SkipWorkload/SkipDDL
// booleans drive the two-phase schema-then-data pattern.
type RunSpec struct {
	JarPath  string
	Workload Workload
	// Hosts are the backing node hostnames/IPs; the port is picked per workload
	// (YSQLPort for Sql*, YCQLPort for Cassandra*).
	Hosts    []string
	YSQLPort int // 0 → DefaultYSQLPort
	YCQLPort int // 0 → DefaultYCQLPort

	NumTables       int
	NumValueColumns int
	NumThreadsRead  int
	NumThreadsWrite int
	RunTime         time.Duration // → --run_time in whole seconds
	UUID            string

	SkipWorkload bool // schema-only: create tables, no data
	SkipDDL      bool // data-only: use existing tables

	SSLCert string            // → --ssl_cert (empty: omitted)
	JVMOpts []string          // extra `java` options, verbatim
	Props   map[string]string // extra --key value flags, deterministically ordered

	// LogPath is where the jar's stdout+stderr is captured on the target.
	// Empty → /tmp/ampqa-sample-app-<marker>.log.
	LogPath string
	// Marker uniquely identifies this launch for pgrep/pkill. Empty → UUID, then
	// "sampleapp". Keep it seed-derived (never wall-clock) for reproducibility.
	Marker string
}

func (s RunSpec) ysqlPort() int {
	if s.YSQLPort != 0 {
		return s.YSQLPort
	}
	return DefaultYSQLPort
}

func (s RunSpec) ycqlPort() int {
	if s.YCQLPort != 0 {
		return s.YCQLPort
	}
	return DefaultYCQLPort
}

func (s RunSpec) port() int {
	if s.Workload.isYCQL() {
		return s.ycqlPort()
	}
	return s.ysqlPort()
}

func (s RunSpec) marker() string {
	if s.Marker != "" {
		return s.Marker
	}
	if s.UUID != "" {
		return s.UUID
	}
	return "sampleapp"
}

func (s RunSpec) logPath() string {
	if s.LogPath != "" {
		return s.LogPath
	}
	return "/tmp/ampqa-sample-app-" + s.marker() + ".log"
}

// nodes renders the comma-separated host:port list for --nodes.
func (s RunSpec) nodes() string {
	port := s.port()
	parts := make([]string, len(s.Hosts))
	for i, h := range s.Hosts {
		parts[i] = fmt.Sprintf("%s:%d", h, port)
	}
	return strings.Join(parts, ",")
}

// BuildCommand renders the background `nohup java … &` launch line. Pure and
// exported so tests assert the exact command without any exec.
func BuildCommand(spec RunSpec) string {
	var b strings.Builder
	b.WriteString("nohup java")
	for _, o := range spec.JVMOpts {
		b.WriteString(" " + o)
	}
	fmt.Fprintf(&b, " -D%s=%s", markerProp, shellQuote(spec.marker()))
	fmt.Fprintf(&b, " -jar %s", shellQuote(spec.JarPath))
	fmt.Fprintf(&b, " --workload %s", shellQuote(string(spec.Workload)))

	if spec.NumTables > 0 {
		fmt.Fprintf(&b, " --num_tables %d", spec.NumTables)
	}
	if spec.NumValueColumns > 0 {
		fmt.Fprintf(&b, " --num_value_columns %d", spec.NumValueColumns)
	}
	if spec.NumThreadsRead > 0 {
		fmt.Fprintf(&b, " --num_threads_read %d", spec.NumThreadsRead)
	}
	if spec.NumThreadsWrite > 0 {
		fmt.Fprintf(&b, " --num_threads_write %d", spec.NumThreadsWrite)
	}
	if spec.RunTime > 0 {
		fmt.Fprintf(&b, " --run_time %d", int(spec.RunTime.Round(time.Second)/time.Second))
	}
	if spec.UUID != "" {
		fmt.Fprintf(&b, " --uuid %s", shellQuote(spec.UUID))
	}
	// Booleans are bare presence flags — emitted only when set, matching the jar.
	if spec.SkipWorkload {
		b.WriteString(" --skip_workload")
	}
	if spec.SkipDDL {
		b.WriteString(" --skip_ddl")
	}
	for _, k := range sortedKeys(spec.Props) {
		fmt.Fprintf(&b, " --%s %s", k, shellQuote(spec.Props[k]))
	}
	if spec.SSLCert != "" {
		fmt.Fprintf(&b, " --ssl_cert %s", shellQuote(spec.SSLCert))
	}
	fmt.Fprintf(&b, " --nodes %s", shellQuote(spec.nodes()))
	fmt.Fprintf(&b, " > %s 2>&1 &", shellQuote(spec.logPath()))
	return b.String()
}

// artifactDeployer is the subset of *artifact.Deployer the app uses; injectable
// so tests assert the deploy path without a real fetch/SSH.
type artifactDeployer interface {
	Deploy(ctx context.Context, execs []exec.RemoteExecutor, spec artifact.Spec, opts artifact.DeployOptions) error
}

// DeploySpec optionally delivers the sample-app jar to the target before Start.
type DeploySpec struct {
	Artifact  artifact.Spec
	RemoteDir string
	Secret    string
}

// App is a running (or runnable) sample-app workload over one RemoteExecutor.
type App struct {
	ex           exec.RemoteExecutor
	spec         RunSpec
	deployer     artifactDeployer
	deploy       *DeploySpec
	logf         func(format string, args ...any)
	readyTimeout time.Duration
	// loadMarker, when set, is the substring WaitReady waits to see in the log
	// before declaring load "going"; empty → a non-empty log plus a live process
	// suffices (no assumption about the jar's exact progress-line format).
	loadMarker string
}

// Option customizes an App.
type Option func(*App)

// WithDeployer injects the artifact deployer used for WithDeploy.
func WithDeployer(d artifactDeployer) Option { return func(a *App) { a.deployer = d } }

// WithDeploy delivers the jar to the target during Start.
func WithDeploy(d DeploySpec) Option { return func(a *App) { a.deploy = &d } }

// WithLogf injects the log sink (default log.Printf).
func WithLogf(logf func(string, ...any)) Option { return func(a *App) { a.logf = logf } }

// WithReadyTimeout bounds WaitReady (default DefaultReadyTimeout).
func WithReadyTimeout(d time.Duration) Option { return func(a *App) { a.readyTimeout = d } }

// WithLoadMarker sets the log substring WaitReady waits for.
func WithLoadMarker(s string) Option { return func(a *App) { a.loadMarker = s } }

// New builds a sample-app App over ex with spec.
func New(ex exec.RemoteExecutor, spec RunSpec, opts ...Option) *App {
	a := &App{
		ex:           ex,
		spec:         spec,
		deployer:     &artifact.Deployer{},
		logf:         log.Printf,
		readyTimeout: DefaultReadyTimeout,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

func (a *App) log(format string, args ...any) {
	if a.logf != nil {
		a.logf(format, args...)
	}
}

// Start deploys the jar (when configured) and launches the background workload.
func (a *App) Start(ctx context.Context) error {
	if len(a.spec.Hosts) == 0 {
		return fmt.Errorf("sampleapp start: no backing hosts")
	}
	if a.spec.JarPath == "" {
		return fmt.Errorf("sampleapp start: empty JarPath")
	}
	if a.deploy != nil {
		if err := a.deployer.Deploy(ctx, []exec.RemoteExecutor{a.ex}, a.deploy.Artifact, artifact.DeployOptions{
			RemoteDir: a.deploy.RemoteDir,
			Secret:    a.deploy.Secret,
		}); err != nil {
			return fmt.Errorf("sampleapp deploy jar: %w", err)
		}
	} else if err := a.checkJar(ctx); err != nil {
		return err
	}
	if err := a.runOK(ctx, BuildCommand(a.spec)); err != nil {
		return fmt.Errorf("sampleapp launch: %w", err)
	}
	return nil
}

// checkJar detects a missing jar on the target and returns missingJarHint,
// rather than a bare launch failure downstream.
func (a *App) checkJar(ctx context.Context) error {
	res, err := a.ex.Run(ctx, fmt.Sprintf("test -f %s", shellQuote(a.spec.JarPath)))
	if err != nil {
		return fmt.Errorf("sampleapp: probe jar: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s", missingJarHint)
	}
	return nil
}

// pgrepPattern is the self-safe marker pattern for pgrep/pkill.
func (a *App) pgrepPattern() string {
	return selfSafePattern(markerProp + "=" + a.spec.marker())
}

// Running reports whether the workload process is alive on the target.
func (a *App) Running(ctx context.Context) (bool, error) {
	res, err := a.ex.Run(ctx, fmt.Sprintf("pgrep -f %s >/dev/null", shellQuote(a.pgrepPattern())))
	if err != nil {
		return false, fmt.Errorf("sampleapp: pgrep: %w", err)
	}
	return res.ExitCode == 0, nil
}

// WaitReady polls until the workload is generating load (the "wait for load is
// going" step) — the process is alive and the log has content (or contains the
// configured load marker) — or ctx/the ready timeout elapses. A process that
// dies before load starts fails fast with the log tail folded in.
func (a *App) WaitReady(ctx context.Context) error {
	wctx, cancel := context.WithTimeout(ctx, a.readyTimeout)
	defer cancel()
	for {
		select {
		case <-wctx.Done():
			tail := a.logTail(ctx)
			return fmt.Errorf("sampleapp %s: load never started within %s: %w (log tail: %s)",
				a.spec.Workload, a.readyTimeout, wctx.Err(), tail)
		default:
		}
		alive, err := a.Running(wctx)
		if err == nil {
			if !alive {
				tail := a.logTail(ctx)
				return fmt.Errorf("sampleapp %s: process exited before load started (log tail: %s)", a.spec.Workload, tail)
			}
			ready, rerr := a.loadStarted(wctx)
			if rerr == nil && ready {
				return nil
			}
		}
		select {
		case <-wctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

// loadStarted reports whether the log signals load is underway.
func (a *App) loadStarted(ctx context.Context) (bool, error) {
	if a.loadMarker != "" {
		res, err := a.ex.Run(ctx, fmt.Sprintf("grep -qF -- %s %s", shellQuote(a.loadMarker), shellQuote(a.spec.logPath())))
		if err != nil {
			return false, err
		}
		return res.ExitCode == 0, nil
	}
	res, err := a.ex.Run(ctx, fmt.Sprintf("test -s %s", shellQuote(a.spec.logPath())))
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

func (a *App) logTail(ctx context.Context) string {
	res, err := a.ex.Run(ctx, fmt.Sprintf("tail -n 20 %s 2>/dev/null", shellQuote(a.spec.logPath())))
	if err != nil {
		return "<unavailable>"
	}
	return strings.TrimSpace(res.Stdout)
}

// Collect pulls the workload log off the target into the sink. Best-effort: an
// empty/unreadable log is logged and skipped, never fatal (collect must not be
// the reason a teardown loses the ability to stop).
func (a *App) Collect(ctx context.Context, into apps.ArtifactSink) error {
	res, err := a.ex.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null", shellQuote(a.spec.logPath())))
	if err != nil {
		a.log("sampleapp collect: %v", err)
		return nil
	}
	if strings.TrimSpace(res.Stdout) == "" {
		return nil
	}
	name := fmt.Sprintf("sample-app-%s.log", a.spec.marker())
	return into.Add(name, "log", []byte(res.Stdout))
}

// Stop kills the workload process (best-effort; a dead/absent process is fine).
func (a *App) Stop(ctx context.Context) error {
	cmd := fmt.Sprintf("pkill -9 -f %s || true", shellQuote(a.pgrepPattern()))
	if err := a.runOK(ctx, cmd); err != nil {
		a.log("sampleapp: stop (best-effort) failed: %v", err)
	}
	return nil
}

// Signal maps a lifecycle signal onto the workload: Stop and Kill both halt it.
func (a *App) Signal(ctx context.Context, sig apps.AppSignal) error {
	switch sig {
	case apps.SignalStop, apps.SignalKill:
		return a.Stop(ctx)
	default:
		return nil
	}
}

// ReportKill collects the workload log and THEN stops the process
// (apps.StopWithCollect) — the report_kill rule: never kill a workload before
// its evidence is safe. This is the correct teardown for the app.
func (a *App) ReportKill(ctx context.Context, into apps.ArtifactSink) error {
	return apps.StopWithCollect(ctx, into, a.Collect, a.Stop)
}

// Close stops the workload (best-effort). Prefer ReportKill so the log is
// collected first.
func (a *App) Close(ctx context.Context) error { return a.Stop(ctx) }

func (a *App) runOK(ctx context.Context, cmd string) error {
	res, err := a.ex.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("command exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// --- config ---

// Getter is the subset of core.Config the FromConfig reader needs.
type Getter interface {
	GetString(key, def string) string
	GetInt(key string, def int) int
	GetBool(key string, def bool) bool
	GetDuration(key string, def time.Duration) time.Duration
}

// Config is the resolved apps.sampleapp.* configuration.
type Config struct {
	Workload        Workload
	NumTables       int
	NumValueColumns int
	NumThreadsRead  int
	NumThreadsWrite int
	RunTime         time.Duration
	UUID            string
	SkipWorkload    bool
	SkipDDL         bool
	YSQLPort        int
	YCQLPort        int
}

// FromConfig reads apps.sampleapp.* into a Config, applying the canonical
// defaults.
func FromConfig(get Getter) Config {
	return Config{
		Workload:        Workload(get.GetString("apps.sampleapp.workload", DefaultWorkload)),
		NumTables:       get.GetInt("apps.sampleapp.num_tables", DefaultNumTables),
		NumValueColumns: get.GetInt("apps.sampleapp.num_value_columns", 0),
		NumThreadsRead:  get.GetInt("apps.sampleapp.num_threads_read", 0),
		NumThreadsWrite: get.GetInt("apps.sampleapp.num_threads_write", 0),
		RunTime:         get.GetDuration("apps.sampleapp.run_time", 0),
		UUID:            get.GetString("apps.sampleapp.uuid", ""),
		SkipWorkload:    get.GetBool("apps.sampleapp.skip_workload", false),
		SkipDDL:         get.GetBool("apps.sampleapp.skip_ddl", false),
		YSQLPort:        get.GetInt("apps.sampleapp.ysql_port", DefaultYSQLPort),
		YCQLPort:        get.GetInt("apps.sampleapp.ycql_port", DefaultYCQLPort),
	}
}

// ApplyTo overlays cfg onto spec, filling the workload/knob fields from config
// while leaving the caller-supplied JarPath/Hosts/Marker untouched.
func (cfg Config) ApplyTo(spec RunSpec) RunSpec {
	spec.Workload = cfg.Workload
	spec.NumTables = cfg.NumTables
	spec.NumValueColumns = cfg.NumValueColumns
	spec.NumThreadsRead = cfg.NumThreadsRead
	spec.NumThreadsWrite = cfg.NumThreadsWrite
	spec.RunTime = cfg.RunTime
	spec.UUID = cfg.UUID
	spec.SkipWorkload = cfg.SkipWorkload
	spec.SkipDDL = cfg.SkipDDL
	spec.YSQLPort = cfg.YSQLPort
	spec.YCQLPort = cfg.YCQLPort
	return spec
}

// --- shared helpers ---

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// selfSafePattern brackets the first byte of a pgrep/pkill pattern
// ("ampqa…" → "[a]mpqa…") so `pkill -f <pattern>` matches the real process but
// never pkill's own argv, which contains the literal bracketed string — without
// this pkill can match and SIGKILL itself.
func selfSafePattern(pat string) string {
	if pat == "" {
		return pat
	}
	return "[" + pat[:1] + "]" + pat[1:]
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
