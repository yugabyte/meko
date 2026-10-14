// Package jepsen drives the yugabyte/jepsen suite REMOTELY over an
// exec.RemoteExecutor — the client-pod rule: the workload never runs on the
// coordinator. It is the ampqa analog of the stress framework's
// src/apps/jepsen/jepsen.py.
//
// One run is a background `nohup lein run test --workload <W> --nodes <hosts> …`
// process inside a clojure/lein container, with jepsen's OWN nemesis disabled
// (--nemesis none): faults are injected by framework/chaos, so the seed that
// drives them is the framework's and the actions are the ones amp understands.
//
// Lifecycle mirrors the stress driver's log-scraping contract, because lein
// reports progress only to its log: Setup (clone + deps) → Start → WaitReady
// ("Setup successful") → WaitAnalysis ("Analysis complete") → Valid (a positive
// "Everything looks good" or :valid? true) → Collect (the store, capped) → ReportKill (collect the
// store BEFORE killing, apps.StopWithCollect — the report_kill lesson).
package jepsen

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps"
	"github.com/yugabyte/amp/test/functional/framework/exec"
)

// Workload is the value passed to lein's --workload flag: an "<api>/<name>"
// pair, where a YSQL name carries the isolation prefix the suite runs it at
// ("sz." serializable, "si." snapshot, "rc." read committed).
type Workload string

// The workloads the suite exposes, mirroring JepsenWorkload.workloads in
// src/apps/jepsen/jepsen.py. Any other name works as a bare Workload too.
const (
	YCQLCounter       Workload = "ycql/counter"
	YCQLSet           Workload = "ycql/set"
	YCQLSetIndex      Workload = "ycql/set-index"
	YCQLBank          Workload = "ycql/bank"
	YCQLLongFork      Workload = "ycql/long-fork"
	YCQLSingleKeyACID Workload = "ycql/single-key-acid"
	YCQLMultiKeyACID  Workload = "ycql/multi-key-acid"

	SZCounter        Workload = "ysql/sz.counter"
	SZSet            Workload = "ysql/sz.set"
	SZBank           Workload = "ysql/sz.bank"
	SZBankContention Workload = "ysql/sz.bank-contention"
	SZBankMultitable Workload = "ysql/sz.bank-multitable"
	SZLongFork       Workload = "ysql/sz.long-fork"
	SZSingleKeyACID  Workload = "ysql/sz.single-key-acid"
	SZMultiKeyACID   Workload = "ysql/sz.multi-key-acid"
	SZDefaultValue   Workload = "ysql/sz.default-value"
	SZAppend         Workload = "ysql/sz.ol.append"

	SIAppendOptimistic  Workload = "ysql/si.ol.append"
	SIAppendPessimistic Workload = "ysql/si.pl.append"
	SIBank              Workload = "ysql/si.bank"
	SIBankContention    Workload = "ysql/si.bank-contention"
	SIBankMultitable    Workload = "ysql/si.bank-multitable"

	RCAppendOptimistic  Workload = "ysql/rc.ol.append"
	RCAppendPessimistic Workload = "ysql/rc.pl.append"
)

// catalog is the full list, in the suite's own order so a seed-driven or
// index-based selection over it is reproducible.
var catalog = []Workload{
	YCQLCounter, YCQLSet, YCQLSetIndex, YCQLBank, YCQLLongFork, YCQLSingleKeyACID, YCQLMultiKeyACID,
	SZCounter, SZSet, SZBank, SZBankContention, SZBankMultitable, SZLongFork, SZSingleKeyACID,
	SZMultiKeyACID, SZDefaultValue, SZAppend,
	SIAppendOptimistic, SIAppendPessimistic, SIBank, SIBankContention, SIBankMultitable,
	RCAppendOptimistic, RCAppendPessimistic,
}

// All returns every known workload.
func All() []Workload { return append([]Workload(nil), catalog...) }

// filter returns the workloads whose name contains sub, preserving order —
// the JepsenWorkload._filter_workloads contract.
func filter(sub string, in []Workload) []Workload {
	if in == nil {
		in = catalog
	}
	out := make([]Workload, 0, len(in))
	for _, w := range in {
		if strings.Contains(string(w), sub) {
			out = append(out, w)
		}
	}
	return out
}

// YSQL returns the YSQL workloads (the set amp can run: amp's compute speaks
// Postgres, so the YCQL half of the suite has no target here).
func YSQL() []Workload { return filter("ysql/", nil) }

// YCQL returns the YCQL workloads.
func YCQL() []Workload { return filter("ycql/", nil) }

// Serializable/SnapshotIsolation/ReadCommitted slice the YSQL set by the
// isolation level its name encodes.
func Serializable() []Workload      { return filter("sz.", YSQL()) }
func SnapshotIsolation() []Workload { return filter("si.", YSQL()) }
func ReadCommitted() []Workload     { return filter("rc.", YSQL()) }

// Filter exposes the substring filter for a caller selecting by config
// (stress.jepsen.workloads entries are matched against the catalog this way).
func Filter(sub string) []Workload { return filter(sub, nil) }

// IsBank/IsSet report the two families whose resource profile differs enough
// that the stress suite overrides concurrency and duration for them: a set or
// long-fork history OOMs the analyser at full concurrency, and bank's ten
// accounts are pure contention past a handful of threads.
func (w Workload) IsBank() bool { return strings.Contains(string(w), "bank") }
func (w Workload) IsSet() bool {
	return strings.Contains(string(w), "set") || strings.Contains(string(w), "long-fork")
}

// IsMultiKeyACID reports multi-key-acid, whose knossos linearizability search
// grows exponentially with concurrent ops: at 80n it outran a 40m analysis.
func (w Workload) IsMultiKeyACID() bool { return strings.Contains(string(w), "multi-key-acid") }

// FileName renders the workload as a filesystem-safe token ("ysql/sz.ol.append"
// → "ysql__sz_ol_append"), matching the stress driver's log naming.
func (w Workload) FileName() string {
	return strings.NewReplacer("/", "__", ".", "_").Replace(string(w))
}

// Log markers. lein reports only to its log, so every lifecycle question is
// answered by scraping these — exactly as the stress driver does.
const (
	MarkerSetupOK      = "Setup successful"
	MarkerAnalysisDone = "Analysis complete"
	MarkerInvalid      = "Analysis invalid"
	MarkerCrashed      = "Test crashed!"
	// MarkerValid is jepsen's success line. Checked POSITIVELY: the absence of
	// MarkerInvalid is not a pass, because a checker that errored out prints
	// MarkerUnknown and accuses the history of nothing either way.
	MarkerValid = "Everything looks good"
	// MarkerUnknown is "Errors occurred during analysis, but no anomalies
	// found" — :valid? :unknown.
	MarkerUnknown = "no anomalies found"
)

// Defaults (mirrored in configs/default.yaml under apps.jepsen.*).
const (
	DefaultRepoURL     = "https://github.com/yugabyte/jepsen"
	DefaultRepoRef     = "yba"
	DefaultRepoDir     = "/opt/ampqa/jepsen"
	DefaultConcurrency = "80n"
	DefaultTimeLimit   = 10 * time.Minute

	DefaultSetupTimeout    = 10 * time.Minute
	DefaultAnalysisTimeout = 20 * time.Minute
	DefaultPollInterval    = 2 * time.Second

	// DefaultCollectMaxBytes caps each collected file. An append history runs to
	// hundreds of MiB; the framework streams and never buffers unbounded (§4),
	// so a collected file is the TAIL of that cap and says so in its header.
	DefaultCollectMaxBytes = 10 << 20

	// markerVar is the environment variable carrying the per-run marker, so
	// Running/Stop/Signal target exactly this launch rather than any `lein` on
	// the pod. It is matched in /proc/<pid>/environ, never argv: the entry is
	// inherited down sh → nohup → lein → java, and appears in none of their argv.
	markerVar = "AMPQA_JEPSEN_MARKER"
)

// SetupSpec is the one-time preparation of the jepsen checkout on the target.
type SetupSpec struct {
	RepoURL string // "" → DefaultRepoURL
	RepoRef string // branch or commit SHA; "" → DefaultRepoRef
	RepoDir string // "" → DefaultRepoDir
}

func (s SetupSpec) url() string {
	if s.RepoURL != "" {
		return s.RepoURL
	}
	return DefaultRepoURL
}

func (s SetupSpec) ref() string {
	if s.RepoRef != "" {
		return s.RepoRef
	}
	return DefaultRepoRef
}

func (s SetupSpec) dir() string {
	if s.RepoDir != "" {
		return s.RepoDir
	}
	return DefaultRepoDir
}

// RunSpec is one `lein run test` launch.
type RunSpec struct {
	Workload Workload
	// Nodes are the hostnames jepsen connects to. Against amp this is the
	// Service in front of ONE endpoint: amp allows a single writer per branch,
	// so there is no multi-node cluster to name here.
	Nodes       []string
	Concurrency string        // "" → DefaultConcurrency
	TimeLimit   time.Duration // "" → DefaultTimeLimit; rendered in whole seconds

	// RepoDir is the checkout SetupSpec created; "" → DefaultRepoDir.
	RepoDir string

	// The flags that make jepsen drive an ALREADY-RUNNING database it must not
	// install, configure or fault itself. Defaults below are the combination the
	// stress driver uses; they are fields rather than constants so a caller can
	// run the suite in its self-installing mode too.
	Env     string // "" → "yba"
	URL     string // "" → "NONE"
	OS      string // "" → "noop"
	NoSSH   bool   // rendered as --no-ssh when true
	Nemesis string // "" → "none" — faults come from framework/chaos

	// Extra are verbatim additional flags (--table-count, --random-seed, …).
	Extra []string

	// LogPath captures stdout+stderr; "" → <RepoDir>/jepsen-<workload>-<conc>.log.
	LogPath string
	// Marker uniquely identifies this launch for Running/Stop/Signal. Keep it
	// seed-derived, never wall-clock, so a replay targets the same process name.
	Marker string
}

func (s RunSpec) concurrency() string {
	if s.Concurrency != "" {
		return s.Concurrency
	}
	return DefaultConcurrency
}

func (s RunSpec) timeLimit() time.Duration {
	if s.TimeLimit > 0 {
		return s.TimeLimit
	}
	return DefaultTimeLimit
}

func (s RunSpec) repoDir() string {
	if s.RepoDir != "" {
		return s.RepoDir
	}
	return DefaultRepoDir
}

func (s RunSpec) env() string {
	if s.Env != "" {
		return s.Env
	}
	return "yba"
}

func (s RunSpec) url() string {
	if s.URL != "" {
		return s.URL
	}
	return "NONE"
}

func (s RunSpec) os() string {
	if s.OS != "" {
		return s.OS
	}
	return "noop"
}

func (s RunSpec) nemesis() string {
	if s.Nemesis != "" {
		return s.Nemesis
	}
	return "none"
}

func (s RunSpec) marker() string {
	if s.Marker != "" {
		return s.Marker
	}
	return s.Workload.FileName()
}

// LogPathOf is the log this launch writes to.
func (s RunSpec) LogPathOf() string {
	if s.LogPath != "" {
		return s.LogPath
	}
	return fmt.Sprintf("%s/jepsen-%s-%s.log", s.repoDir(), s.Workload.FileName(), s.concurrency())
}

// storeDir is where jepsen writes the run's history and analysis.
func (s RunSpec) storeDir() string { return s.repoDir() + "/yugabyte/store/latest" }

// BuildSetupCommand renders the checkout preparation. Pure and exported so
// tests assert it without any exec.
func BuildSetupCommand(s SetupSpec) string {
	dir, url, ref := s.dir(), s.url(), s.ref()
	// Idempotent: a re-used pod (or a retried setup) must not fail on an
	// existing checkout, and `git fetch` then a detached checkout resolves a
	// branch name and a commit SHA alike.
	//
	// git, gnuplot and graphviz are installed only when missing, guarded on
	// `command -v`, so a pod that already has them is not held up by a package
	// index fetch. The clojure images carry lein and a JDK but not reliably
	// git. An image without apt therefore has to carry all three.
	return strings.Join([]string{
		// gnuplot and graphviz are NOT optional niceties: jepsen's :rate-graph,
		// :latency-graph and :plot checkers shell out to them, and when the
		// binary is missing those checkers throw. The run then reports
		// :valid? :unknown ("errors occurred during analysis") even though every
		// consistency checker passed — a weaker result that is easy to misread
		// as a pass. The stress framework installs both for the same reason
		// (src/apps/jepsen/jepsen.py setup_environment).
		"if ! command -v git >/dev/null || " +
			"! command -v gnuplot >/dev/null || ! command -v dot >/dev/null; then " +
			"apt-get update && apt-get install -y --no-install-recommends git gnuplot graphviz; fi",
		fmt.Sprintf("mkdir -p %s", shellQuote(dir)),
		fmt.Sprintf("if [ ! -d %s ]; then git clone %s %s; fi",
			shellQuote(dir+"/.git"), shellQuote(url), shellQuote(dir)),
		fmt.Sprintf("cd %s", shellQuote(dir)),
		fmt.Sprintf("git fetch --tags --force origin %s", shellQuote(ref)),
		"git checkout --detach FETCH_HEAD",
		fmt.Sprintf("cd %s", shellQuote(dir+"/yugabyte")),
		"lein deps",
	}, " && ")
}

// BuildCommand renders the background `nohup lein run test … &` launch line.
// Pure and exported so tests assert the exact command without any exec.
func BuildCommand(spec RunSpec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "cd %s && ", shellQuote(spec.repoDir()+"/yugabyte"))
	fmt.Fprintf(&b, "%s=%s nohup lein run test", markerVar, shellQuote(spec.marker()))
	fmt.Fprintf(&b, " --nodes %s", shellQuote(strings.Join(spec.Nodes, ",")))
	fmt.Fprintf(&b, " --concurrency %s", shellQuote(spec.concurrency()))
	fmt.Fprintf(&b, " --time-limit %d", int(spec.timeLimit().Round(time.Second)/time.Second))
	fmt.Fprintf(&b, " --workload %s", shellQuote(string(spec.Workload)))
	fmt.Fprintf(&b, " --url %s", shellQuote(spec.url()))
	fmt.Fprintf(&b, " --env %s", shellQuote(spec.env()))
	if spec.NoSSH {
		b.WriteString(" --no-ssh")
	}
	fmt.Fprintf(&b, " --os %s", shellQuote(spec.os()))
	fmt.Fprintf(&b, " --nemesis %s", shellQuote(spec.nemesis()))
	for _, e := range spec.Extra {
		b.WriteString(" " + e)
	}
	fmt.Fprintf(&b, " > %s 2>&1 &", shellQuote(spec.LogPathOf()))
	return b.String()
}

// SQLExecutor is the minimal pg surface SetupDatabase needs (*pg.Client
// satisfies it structurally, so apps/jepsen imports no other app).
type SQLExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (int64, error)
}

// RolePassword is the password the role is created with. It is NOT a choice:
// jepsen hardcodes all three of dbname, user and password as the literal
// "jepsen" (yugabyte/jepsen src/yugabyte/ysql/client.clj — conn-wrapper calls
// open-conn "jepsen" "jepsen" "jepsen"), so a role created any other way cannot
// be connected to.
//
// It has to be set at all because an amp endpoint requires password
// authentication. A password-less role is accepted by CREATE USER and then
// fails every connection with `FATAL: password authentication failed for user
// "jepsen"` — which jepsen reports as a connection timeout from inside its
// retry wrapper, so the endpoint looks unreachable rather than unauthenticated.
const RolePassword = "jepsen"

// SetupDatabase creates the role and database every jepsen workload expects.
//
// Deliberately NOT the stress driver's sequence: that one also runs
// `grant yb_extension to jepsen`, a role YugabyteDB defines and an amp compute
// does not, so the grant would abort the whole setup here.
//
// The drop is retried because a workload's own connections can still hold the
// database open for a moment after the run ends ("is being accessed by other
// users"), exactly as JepsenApp.setup_database works around.
func SetupDatabase(ctx context.Context, db SQLExecutor, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	for _, stmt := range []string{
		"DROP DATABASE IF EXISTS jepsen",
		"DROP USER IF EXISTS jepsen",
		"CREATE USER jepsen CREATEDB PASSWORD '" + RolePassword + "'",
	} {
		if err := retryExec(ctx, db, stmt, timeout); err != nil {
			return err
		}
	}
	return retryExec(ctx, db, "CREATE DATABASE jepsen OWNER jepsen", timeout)
}

// ResetDatabase drops and recreates only the database, between iterations of
// the same workload — the role survives.
func ResetDatabase(ctx context.Context, db SQLExecutor, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	if err := retryExec(ctx, db, "DROP DATABASE IF EXISTS jepsen", timeout); err != nil {
		return err
	}
	return retryExec(ctx, db, "CREATE DATABASE jepsen OWNER jepsen", timeout)
}

func retryExec(ctx context.Context, db SQLExecutor, stmt string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		if _, err := db.Exec(ctx, stmt); err == nil {
			return nil
		} else {
			last = err
		}
		if ctx.Err() != nil {
			return fmt.Errorf("jepsen setup %q: %w (last error: %v)", stmt, ctx.Err(), last)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("jepsen setup %q did not succeed within %s: %w", stmt, timeout, last)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("jepsen setup %q: %w (last error: %v)", stmt, ctx.Err(), last)
		case <-time.After(DefaultPollInterval):
		}
	}
}

// App is a running (or runnable) jepsen workload over one RemoteExecutor.
type App struct {
	ex   exec.RemoteExecutor
	spec RunSpec
	logf func(format string, args ...any)

	setupTimeout    time.Duration
	analysisTimeout time.Duration
	pollInterval    time.Duration
	collectMaxBytes int
}

// Option customizes an App.
type Option func(*App)

// WithLogf injects the log sink (default log.Printf).
func WithLogf(logf func(string, ...any)) Option { return func(a *App) { a.logf = logf } }

// WithSetupTimeout bounds WaitReady (default DefaultSetupTimeout).
func WithSetupTimeout(d time.Duration) Option { return func(a *App) { a.setupTimeout = d } }

// WithAnalysisTimeout bounds WaitAnalysis (default DefaultAnalysisTimeout).
func WithAnalysisTimeout(d time.Duration) Option { return func(a *App) { a.analysisTimeout = d } }

// WithPollInterval sets the log-scrape cadence (default DefaultPollInterval).
func WithPollInterval(d time.Duration) Option { return func(a *App) { a.pollInterval = d } }

// WithCollectMaxBytes caps each collected file (default DefaultCollectMaxBytes).
func WithCollectMaxBytes(n int) Option { return func(a *App) { a.collectMaxBytes = n } }

// New builds a jepsen App over ex with spec.
func New(ex exec.RemoteExecutor, spec RunSpec, opts ...Option) *App {
	a := &App{
		ex:              ex,
		spec:            spec,
		logf:            log.Printf,
		setupTimeout:    DefaultSetupTimeout,
		analysisTimeout: DefaultAnalysisTimeout,
		pollInterval:    DefaultPollInterval,
		collectMaxBytes: DefaultCollectMaxBytes,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Spec returns the launch this App drives.
func (a *App) Spec() RunSpec { return a.spec }

func (a *App) log(format string, args ...any) {
	if a.logf != nil {
		a.logf(format, args...)
	}
}

// Setup prepares the checkout on the target. Safe to call on a pod that
// already has one.
func (a *App) Setup(ctx context.Context, s SetupSpec) error {
	if err := a.runOK(ctx, BuildSetupCommand(s)); err != nil {
		return fmt.Errorf("jepsen setup checkout: %w", err)
	}
	return nil
}

// Start launches the background workload.
func (a *App) Start(ctx context.Context) error {
	if len(a.spec.Nodes) == 0 {
		return fmt.Errorf("jepsen start: no nodes")
	}
	if a.spec.Workload == "" {
		return fmt.Errorf("jepsen start: empty workload")
	}
	if err := a.runOK(ctx, BuildCommand(a.spec)); err != nil {
		return fmt.Errorf("jepsen launch %s: %w", a.spec.Workload, err)
	}
	return nil
}

// WaitReady blocks until jepsen logs that its SETUP — the workload's DDL and
// initial data — is done. Nothing may fault the system before this returns:
// a fault during setup fails the run for a schema that never got created, not
// for anything the workload was asking about.
func (a *App) WaitReady(ctx context.Context) error {
	return a.awaitMarker(ctx, MarkerSetupOK, a.setupTimeout, "setup")
}

// WaitAnalysis blocks until jepsen has finished checking the history.
func (a *App) WaitAnalysis(ctx context.Context) error {
	return a.awaitMarker(ctx, MarkerAnalysisDone, a.analysisTimeout, "analysis")
}

// awaitMarker polls the log for want, failing fast if jepsen crashed.
func (a *App) awaitMarker(ctx context.Context, want string, timeout time.Duration, phase string) error {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		crashed, err := a.hasMarker(wctx, MarkerCrashed)
		if err == nil && crashed {
			return fmt.Errorf("jepsen %s: test crashed during %s (log tail: %s)",
				a.spec.Workload, phase, a.LogTail(ctx))
		}
		found, err := a.hasMarker(wctx, want)
		if err == nil && found {
			return nil
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("jepsen %s: %s did not complete within %s: %w (log tail: %s)",
				a.spec.Workload, phase, timeout, wctx.Err(), a.LogTail(ctx))
		case <-time.After(a.pollInterval):
		}
	}
}

// hasMarker reports whether the log contains s.
func (a *App) hasMarker(ctx context.Context, s string) (bool, error) {
	cmd := fmt.Sprintf("grep -qF -- %s %s", shellQuote(s), shellQuote(a.spec.LogPathOf()))
	res, err := a.ex.Run(ctx, cmd)
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

// Valid reports whether the analysis passed: a positive "Everything looks
// good" or :valid? true. :unknown is not a pass. Call it only after
// WaitAnalysis: before that the log is simply silent on the question, and
// silence is not a pass.
func (a *App) Valid(ctx context.Context) (bool, error) {
	v, err := a.Verdict(ctx)
	if err != nil {
		return false, err
	}
	return v == VerdictValid, nil
}

// Verdict is jepsen's own three-way answer, which is NOT a boolean: a checker
// that could not finish reports :valid? :unknown, and jepsen says so with its
// own closing line. Reading only for "Analysis invalid" collapses unknown into
// a pass — the failure mode this type exists to prevent.
type Verdict string

const (
	VerdictValid   Verdict = "valid"
	VerdictInvalid Verdict = "invalid"
	// VerdictUnknown is "errors occurred during analysis, but no anomalies
	// found": the history is not accused of anything, but some checker did not
	// complete, so the run proves less than it appears to.
	VerdictUnknown Verdict = "unknown"
)

// Verdict reads the closing line jepsen prints after the analysis.
func (a *App) Verdict(ctx context.Context) (Verdict, error) {
	done, err := a.hasMarker(ctx, MarkerAnalysisDone)
	if err != nil {
		return "", fmt.Errorf("jepsen %s: read verdict: %w", a.spec.Workload, err)
	}
	if !done {
		return "", fmt.Errorf("jepsen %s: no %q in the log, so there is no verdict to read",
			a.spec.Workload, MarkerAnalysisDone)
	}
	// Order matters: an invalid run is the finding, so it wins over anything
	// else the log happens to contain.
	for _, c := range []struct {
		marker  string
		verdict Verdict
	}{
		{MarkerInvalid, VerdictInvalid},
		{MarkerUnknown, VerdictUnknown},
		{MarkerValid, VerdictValid},
	} {
		hit, err := a.hasMarker(ctx, c.marker)
		if err != nil {
			return "", fmt.Errorf("jepsen %s: read verdict: %w", a.spec.Workload, err)
		}
		if hit {
			return c.verdict, nil
		}
	}
	// jepsen prints the verdict line a moment after "Analysis complete", so a
	// read that lands between the two finds neither; results.edn already holds
	// the same verdict as its top-level :valid? (run 725dda44).
	if v, ok := a.resultsVerdict(ctx); ok {
		return v, nil
	}
	return "", fmt.Errorf("jepsen %s: the analysis finished but printed none of the three verdict lines",
		a.spec.Workload)
}

// resultsTail matches the top-level :valid? that closes results.edn.
var resultsTail = regexp.MustCompile(`:valid\? (true|false|:unknown)\}\s*$`)

// resultsVerdict reads the verdict from the store's results.edn.
func (a *App) resultsVerdict(ctx context.Context) (Verdict, bool) {
	res, err := a.ex.Run(ctx, fmt.Sprintf("tail -c 400 %s 2>/dev/null", shellQuote(a.spec.storeDir()+"/results.edn")))
	if err != nil || res.ExitCode != 0 {
		return "", false
	}
	m := resultsTail.FindStringSubmatch(res.Stdout)
	if m == nil {
		return "", false
	}
	switch m[1] {
	case "true":
		return VerdictValid, true
	case "false":
		return VerdictInvalid, true
	default:
		return VerdictUnknown, true
	}
}

// Running reports whether the workload process is alive on the target.
func (a *App) Running(ctx context.Context) (bool, error) {
	res, err := a.ex.Run(ctx, fmt.Sprintf(`[ -n "$(%s)" ]`, a.pidsCmd()))
	if err != nil {
		return false, fmt.Errorf("jepsen: find the run's processes: %w", err)
	}
	return res.ExitCode == 0, nil
}

// markerEntry is the environment entry BuildCommand sets on the launch.
func (a *App) markerEntry() string { return markerVar + "=" + a.spec.marker() }

// pidsCmd prints the PIDs whose environment carries this launch's marker. The
// grep's own environment does not, so it never lists itself.
func (a *App) pidsCmd() string {
	return fmt.Sprintf("grep -lszFx -- %s /proc/[0-9]*/environ | sed 's#^/proc/##; s#/environ$##'",
		shellQuote(a.markerEntry()))
}

// signalCmd sends sig to every process of this launch; finding none is fine.
func (a *App) signalCmd(sig string) string {
	return fmt.Sprintf(`pids=$(%s); [ -z "$pids" ] || kill -%s $pids || true`, a.pidsCmd(), sig)
}

// LogTail returns the recent output of the run, for an error message.
func (a *App) LogTail(ctx context.Context) string {
	res, err := a.ex.Run(ctx, fmt.Sprintf("tail -n 30 %s 2>/dev/null", shellQuote(a.spec.LogPathOf())))
	if err != nil {
		return "<unavailable>"
	}
	return strings.TrimSpace(res.Stdout)
}

// Collect pulls the run's log and its store directory into the sink. An elle
// analysis writes a subdirectory of its own, which is walked one level deeper —
// those files are the evidence for an "Analysis invalid" verdict.
//
// Best-effort per file: an unreadable one is logged and skipped, never fatal
// (collect must not be the reason a teardown loses the ability to stop). Each
// file is capped at collectMaxBytes and marked when truncated.
func (a *App) Collect(ctx context.Context, into apps.ArtifactSink) error {
	prefix := "jepsen/" + a.spec.Workload.FileName()
	a.collectFile(ctx, into, a.spec.LogPathOf(), prefix+"/launch.log")

	files, err := a.listFiles(ctx, a.spec.storeDir())
	if err != nil {
		a.log("jepsen collect: list store: %v", err)
		return nil
	}
	for _, f := range files {
		a.collectFile(ctx, into, a.spec.storeDir()+"/"+f, prefix+"/"+f)
	}
	return nil
}

// CollectMainLogOnly collects just the run log, for a workload that PASSED:
// the full store of a passing append run is hundreds of MiB of history nobody
// will read. Mirrors report_logs(main_log_only=True).
func (a *App) CollectMainLogOnly(ctx context.Context, into apps.ArtifactSink) error {
	prefix := "jepsen/" + a.spec.Workload.FileName()
	a.collectFile(ctx, into, a.spec.LogPathOf(), prefix+"/launch.log")
	a.collectFile(ctx, into, a.spec.storeDir()+"/jepsen.log", prefix+"/jepsen.log")
	return nil
}

// listFiles returns the regular files under dir, one level into any
// subdirectory (an elle analysis writes one).
func (a *App) listFiles(ctx context.Context, dir string) ([]string, error) {
	cmd := fmt.Sprintf("cd %s 2>/dev/null && find . -maxdepth 2 -type f -printf '%%P\\n' 2>/dev/null | sort",
		shellQuote(dir))
	res, err := a.ex.Run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("no store directory at %s (exit %d)", dir, res.ExitCode)
	}
	var out []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		if l := strings.TrimSpace(line); l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

// collectFile copies one remote file into the sink, capped.
func (a *App) collectFile(ctx context.Context, into apps.ArtifactSink, remote, name string) {
	size, err := a.fileSize(ctx, remote)
	if err != nil || size == 0 {
		return
	}
	cmd := fmt.Sprintf("tail -c %d %s 2>/dev/null", a.collectMaxBytes, shellQuote(remote))
	res, err := a.ex.Run(ctx, cmd)
	if err != nil {
		a.log("jepsen collect %s: %v", remote, err)
		return
	}
	body := res.Stdout
	switch {
	case res.Truncated:
		// The exec capture cap cut the tail short: these are its first bytes, not
		// its last, so the header must not claim "last N".
		body = fmt.Sprintf("[ampqa: truncated — the first %d bytes of the last %d of %d, cut by the exec capture cap]\n%s",
			len(res.Stdout), a.collectMaxBytes, size, body)
	case size > int64(a.collectMaxBytes):
		body = fmt.Sprintf("[ampqa: truncated — last %d of %d bytes]\n%s", a.collectMaxBytes, size, body)
	}
	if err := into.Add(name, "log", []byte(body)); err != nil {
		a.log("jepsen collect %s: sink: %v", name, err)
	}
}

// fileSize returns the byte size of remote, or 0 when it does not exist.
func (a *App) fileSize(ctx context.Context, remote string) (int64, error) {
	res, err := a.ex.Run(ctx, fmt.Sprintf("wc -c < %s 2>/dev/null", shellQuote(remote)))
	if err != nil {
		return 0, err
	}
	if res.ExitCode != 0 {
		return 0, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(res.Stdout), 10, 64)
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// Stop kills the workload process (best-effort; a dead/absent process is fine).
func (a *App) Stop(ctx context.Context) error {
	cmd := a.signalCmd("KILL")
	if err := a.runOK(ctx, cmd); err != nil {
		a.log("jepsen: stop (best-effort) failed: %v", err)
	}
	return nil
}

// CleanStore removes the run's store so the next workload's "latest" is its
// own. Mirrors JepsenApp.cleanup — and MUST run after Collect, never before.
func (a *App) CleanStore(ctx context.Context) error {
	cmd := fmt.Sprintf("rm -rf %s", shellQuote(a.spec.repoDir()+"/yugabyte/store"))
	return a.runOK(ctx, cmd)
}

// Signal maps a lifecycle signal onto the workload. Pause/resume are the
// SIGSTOP/SIGCONT the stress driver uses to hold jepsen still.
func (a *App) Signal(ctx context.Context, sig apps.AppSignal) error {
	switch sig {
	case apps.SignalStop, apps.SignalKill:
		return a.Stop(ctx)
	case apps.SignalPause:
		return a.runOK(ctx, a.signalCmd("STOP"))
	default:
		return nil
	}
}

// Resume continues a paused run (SIGCONT).
func (a *App) Resume(ctx context.Context) error {
	return a.runOK(ctx, a.signalCmd("CONT"))
}

// ReportKill collects the store and THEN stops the process
// (apps.StopWithCollect) — never kill a workload before its evidence is safe.
func (a *App) ReportKill(ctx context.Context, into apps.ArtifactSink) error {
	return apps.StopWithCollect(ctx, into, a.Collect, a.Stop)
}

// Close stops the workload (best-effort). Prefer ReportKill so the store is
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
	GetDuration(key string, def time.Duration) time.Duration
	GetMapSlice(key string, def []map[string]any) []map[string]any
}

// Config is the resolved apps.jepsen.* configuration.
type Config struct {
	Image           string
	RepoURL         string
	RepoRef         string
	RepoDir         string
	Concurrency     string
	TimeLimit       time.Duration
	Repeats         int
	SetupTimeout    time.Duration
	AnalysisTimeout time.Duration
	PollInterval    time.Duration
	CollectMaxBytes int
	Patches         []Patch
}

// DefaultImage is the public clojure/lein image the client pod runs. Public on
// purpose: needing a registry credential for the runner would put the whole
// suite behind a secret it does not otherwise require.
const DefaultImage = "clojure:temurin-21-lein-bookworm"

// FromConfig reads apps.jepsen.* into a Config, applying the canonical
// defaults.
func FromConfig(get Getter) Config {
	return Config{
		Image:           get.GetString("apps.jepsen.image", DefaultImage),
		RepoURL:         get.GetString("apps.jepsen.repo_url", DefaultRepoURL),
		RepoRef:         get.GetString("apps.jepsen.repo_ref", DefaultRepoRef),
		RepoDir:         get.GetString("apps.jepsen.repo_dir", DefaultRepoDir),
		Concurrency:     get.GetString("apps.jepsen.concurrency", DefaultConcurrency),
		TimeLimit:       get.GetDuration("apps.jepsen.time_to_run", DefaultTimeLimit),
		Repeats:         get.GetInt("apps.jepsen.repeats", 1),
		SetupTimeout:    get.GetDuration("apps.jepsen.setup_timeout", DefaultSetupTimeout),
		AnalysisTimeout: get.GetDuration("apps.jepsen.analysis_timeout", DefaultAnalysisTimeout),
		PollInterval:    get.GetDuration("apps.jepsen.poll_interval", DefaultPollInterval),
		CollectMaxBytes: get.GetInt("apps.jepsen.collect_max_bytes", DefaultCollectMaxBytes),
		Patches:         PatchesFromConfig(get),
	}
}

// SetupSpec renders the checkout spec this config describes.
func (c Config) SetupSpec() SetupSpec {
	return SetupSpec{RepoURL: c.RepoURL, RepoRef: c.RepoRef, RepoDir: c.RepoDir}
}

// Shape returns the concurrency and duration to run w at, applying the
// per-family overrides the stress suite carries: a set or long-fork history
// OOMs the analyser at full concurrency, a multi-key-acid one is never
// analysed in time, and bank's ten accounts are pure contention past a handful
// of threads.
func (c Config) Shape(w Workload) (concurrency string, limit time.Duration) {
	switch {
	case w.IsSet(), w.IsMultiKeyACID():
		return "10n", 5 * time.Minute
	case w.IsBank():
		return "5n", c.TimeLimit
	default:
		return c.Concurrency, c.TimeLimit
	}
}

// --- shared helpers ---

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
