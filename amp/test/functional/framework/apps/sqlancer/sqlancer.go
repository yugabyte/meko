// Package sqlancer drives SQLancer (the yugabyte fork, branch master-yb)
// REMOTELY over an exec.RemoteExecutor — the client-pod rule: the fuzzer never
// runs on the coordinator.
//
// SQLancer is installed from the fork's master-yb release (sqlancer_2.0.0-yb:
// the jar plus lib/), not built. That release is deleted and recreated on every
// push to master-yb, so the tag moves: Install reads the commit the tag points
// at before and after the download and refuses a download the tag moved under,
// and reports that commit and the tarball's sha256 as the record of what ran.
//
// One run is one oracle: `java -jar sqlancer.jar … amp --oracle <O>` for
// --timeout-seconds, launched detached so a dropped exec stream costs one poll
// rather than the run. The amp provider is the YSQL provider in
// PostgreSQL-compatible mode, so it emits no YugabyteDB-only syntax.
//
// SQLancer's own verdict is the contract. A thread that finds a bug writes the
// reproducer to logs/amp/<database>.log and the process exits with
// --exit-code-error; logs/amp/<database>-cur.log is the in-progress statement
// log every thread keeps and is NOT a finding. Any exception a thread throws
// counts, a dropped connection included — which is why nothing faults the
// stack while this runs.
package sqlancer

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps"
	"github.com/yugabyte/amp/test/functional/framework/exec"
)

// Oracles is every oracle the amp provider accepts, in the order the suite
// runs them by default.
var Oracles = []string{
	"WHERE", "HAVING", "AGGREGATE", "DISTINCT", "GROUP_BY", "QUERY_PARTITIONING",
	"NOREC", "PQS", "CERT", "CODDTEST", "SCAN_GUC", "CATALOG", "DQP", "DQE",
	"SKIP_LOCKED", "FUZZER",
}

// Defaults (mirrored in configs/default.yaml under apps.sqlancer.*).
const (
	// A JRE is enough now that nothing is compiled; it carries curl and tar.
	DefaultImage           = "eclipse-temurin:21-jre"
	DefaultReleaseRepo     = "yugabyte/sqlancer"
	DefaultReleaseTag      = "sqlancer_2.0.0-yb"
	DefaultReleaseAsset    = "sqlancer-2.0.0-yb.tar.gz"
	DefaultInstallDir      = "/opt/ampqa/sqlancer"
	DefaultInstallTimeout  = 20 * time.Minute
	DefaultThreads         = 16
	DefaultTimePerOracle   = 10 * time.Minute
	DefaultMaxHeap         = "4g"
	DefaultPollInterval    = 5 * time.Second
	DefaultCollectMaxBytes = 10 << 20
	// exitCodeError is passed as --exit-code-error so a finding is a code of
	// our own choosing, not SQLancer's default -1 (255 in a shell), which a JVM
	// crash can also produce.
	exitCodeError = 3
	// exitGrace is how long past --timeout-seconds the run may take before it is
	// abandoned: threads finish their current statement, and a thread that
	// found a bug writes its log first.
	exitGrace = 5 * time.Minute
)

// A dropped poll must not kill the run, but a target that stopped answering
// entirely has to surface eventually.
const maxConsecutivePollErrors = 24

// Config is the resolved apps.sqlancer.* configuration.
type Config struct {
	Image string
	// ReleaseRepo is the GitHub owner/name that publishes ReleaseTag.
	ReleaseRepo     string
	ReleaseTag      string
	ReleaseAsset    string
	InstallDir      string
	InstallTimeout  time.Duration
	Threads         int
	TimePerOracle   time.Duration
	MaxHeap         string
	PollInterval    time.Duration
	CollectMaxBytes int
	// ExtraArgs are appended to SQLancer's global options, before the provider
	// name, for a run that needs a flag this package does not model.
	ExtraArgs []string
}

// Getter is the subset of core.Config FromConfig needs.
type Getter interface {
	GetString(key, def string) string
	GetInt(key string, def int) int
	GetDuration(key string, def time.Duration) time.Duration
	GetStringSlice(key string, def []string) []string
}

// FromConfig reads apps.sqlancer.*.
func FromConfig(get Getter) Config {
	return Config{
		Image:           get.GetString("apps.sqlancer.image", DefaultImage),
		ReleaseRepo:     get.GetString("apps.sqlancer.release_repo", DefaultReleaseRepo),
		ReleaseTag:      get.GetString("apps.sqlancer.release_tag", DefaultReleaseTag),
		ReleaseAsset:    get.GetString("apps.sqlancer.release_asset", DefaultReleaseAsset),
		InstallDir:      get.GetString("apps.sqlancer.install_dir", DefaultInstallDir),
		InstallTimeout:  get.GetDuration("apps.sqlancer.install_timeout", DefaultInstallTimeout),
		Threads:         get.GetInt("apps.sqlancer.threads", DefaultThreads),
		TimePerOracle:   get.GetDuration("apps.sqlancer.time_per_oracle", DefaultTimePerOracle),
		MaxHeap:         get.GetString("apps.sqlancer.max_heap", DefaultMaxHeap),
		PollInterval:    get.GetDuration("apps.sqlancer.poll_interval", DefaultPollInterval),
		CollectMaxBytes: get.GetInt("apps.sqlancer.collect_max_bytes", DefaultCollectMaxBytes),
		ExtraArgs:       get.GetStringSlice("apps.sqlancer.extra_args", nil),
	}
}

// Target is the endpoint SQLancer connects to. SQLancer creates and drops its
// own databases on it, so the user must be able to (cloud_admin can).
type Target struct {
	Host     string
	Port     int
	User     string
	Password string
}

// RunSpec is one oracle run.
type RunSpec struct {
	Oracle string
	Target Target
	// Jar is the installed jar's path on the target, as Install returned it.
	Jar     string
	WorkDir string
	Threads int
	Timeout time.Duration
	MaxHeap string
	Extra   []string
}

// DatabasePrefix names every database this run creates. Per oracle, so two
// oracles' leftovers never collide and a finding names the oracle it came from.
func (s RunSpec) DatabasePrefix() string {
	return "sqlancer_" + strings.ToLower(s.Oracle) + "_"
}

// LogPath is the run's own stdout/stderr.
func (s RunSpec) LogPath() string { return path.Join(s.WorkDir, "sqlancer.out") }

// ExitPath is where the detached wrapper records the exit code.
func (s RunSpec) ExitPath() string { return path.Join(s.WorkDir, "sqlancer.exit") }

// FindingsDir is where SQLancer writes one reproducer log per failing database.
func (s RunSpec) FindingsDir() string { return path.Join(s.WorkDir, "logs", "amp") }

// exitTagMoved is the install command's exit code when the release tag moved
// while the tarball downloaded.
const exitTagMoved = 65

// BuildInstallCommand renders the download and unpack of the release. Its last
// three lines are the commit the tag points at, the tarball's sha256 and the
// jar path. Pure and exported so tests assert the exact command without any exec.
func BuildInstallCommand(c Config) string {
	dir := c.InstallDir
	tarball := path.Join(dir, c.ReleaseAsset)
	refURL := fmt.Sprintf("https://api.github.com/repos/%s/git/ref/tags/%s", c.ReleaseRepo, c.ReleaseTag)
	assetURL := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", c.ReleaseRepo, c.ReleaseTag, c.ReleaseAsset)
	// The ref document has exactly one "sha": the commit a lightweight tag names.
	tagCommit := fmt.Sprintf(`curl -fsSL %s | sed -n 's/.*"sha": *"\([0-9a-f]\{40\}\)".*/\1/p' | head -n1`,
		shellQuote(refURL))
	return strings.Join([]string{
		fmt.Sprintf("rm -rf %s", shellQuote(dir)),
		fmt.Sprintf("mkdir -p %s", shellQuote(dir)),
		fmt.Sprintf("cd %s", shellQuote(dir)),
		"before=$(" + tagCommit + ")",
		`[ -n "$before" ]`,
		fmt.Sprintf("curl -fsSL --retry 3 -o %s %s", shellQuote(tarball), shellQuote(assetURL)),
		"after=$(" + tagCommit + ")",
		// A failed re-read (rate limit, network) would otherwise read as a move.
		`[ -n "$after" ]`,
		// The release is recreated on every master-yb push, so a tag that moved
		// during the download leaves no way to say which commit the bytes are.
		fmt.Sprintf(`{ [ "$before" = "$after" ] || { echo "release tag moved during download: $before -> $after"; exit %d; }; }`,
			exitTagMoved),
		fmt.Sprintf("sum=$(sha256sum %s | cut -d' ' -f1)", shellQuote(tarball)),
		fmt.Sprintf("tar xzf %s", shellQuote(tarball)),
		fmt.Sprintf("rm -f %s", shellQuote(tarball)),
		`echo "$before"`,
		`echo "$sum"`,
		fmt.Sprintf("ls %s/*.jar | head -n1", shellQuote(dir)),
	}, " && ")
}

// BuildRunCommand renders one oracle run, foreground; BuildStartCommand
// detaches it. A fresh WorkDir each time, so logs/amp holds only this run's
// findings.
func BuildRunCommand(s RunSpec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "rm -rf %s && mkdir -p %s && cd %s && ",
		shellQuote(s.WorkDir), shellQuote(s.WorkDir), shellQuote(s.WorkDir))
	fmt.Fprintf(&b, "java -Xmx%s -jar %s", s.MaxHeap, shellQuote(s.Jar))
	fmt.Fprintf(&b, " --host %s --port %d", shellQuote(s.Target.Host), s.Target.Port)
	fmt.Fprintf(&b, " --username %s --password %s", shellQuote(s.Target.User), shellQuote(s.Target.Password))
	fmt.Fprintf(&b, " --num-threads %d", s.Threads)
	fmt.Fprintf(&b, " --timeout-seconds %d", int(s.Timeout.Round(time.Second)/time.Second))
	fmt.Fprintf(&b, " --database-prefix %s", shellQuote(s.DatabasePrefix()))
	fmt.Fprintf(&b, " --exit-code-error %d", exitCodeError)
	for _, e := range s.Extra {
		b.WriteString(" " + shellQuote(e))
	}
	fmt.Fprintf(&b, " amp --oracle %s", shellQuote(s.Oracle))
	fmt.Fprintf(&b, " > %s 2>&1", shellQuote(path.Base(s.LogPath())))
	return b.String()
}

// BuildStartCommand renders the detached launch of cmd, which records its exit
// code in exitPath; only the wrapper's pid comes back. A multi-minute command
// held on a live exec stream dies with that stream; a detached one survives it.
func BuildStartCommand(cmd, exitPath string) string {
	inner := "(" + cmd + "); echo $? > " + shellQuote(exitPath)
	// ";" not "&&": with "rm && nohup … & echo $!" the "&" grabs the whole list,
	// so $! would be a wrapping subshell and rm would race the first poll.
	return "rm -f " + shellQuote(exitPath) +
		"; nohup sh -c " + shellQuote(inner) + " >/dev/null 2>&1 & echo $!"
}

// Verdict is the outcome of one oracle run.
type Verdict string

const (
	// VerdictClean: SQLancer exited 0 and wrote no reproducer.
	VerdictClean Verdict = "clean"
	// VerdictFindings: at least one reproducer log exists. The exit code is not
	// consulted for this: a run whose threads all failed can still exit on the
	// timeout path, and the logs are the evidence either way.
	VerdictFindings Verdict = "findings"
)

// Result is one oracle run's outcome.
type Result struct {
	Verdict  Verdict
	ExitCode int
	// Findings are the reproducer logs, relative to FindingsDir.
	Findings []string
	// RunError is why a run with findings did not end on its own; empty when it did.
	RunError string
}

// App drives SQLancer on one RemoteExecutor.
type App struct {
	ex   exec.RemoteExecutor
	cfg  Config
	logf func(string, ...any)
}

// New builds an App over ex.
func New(ex exec.RemoteExecutor, cfg Config, logf func(string, ...any)) *App {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &App{ex: ex, cfg: cfg, logf: logf}
}

// Installed is what Install put on the target and the record of which build it is.
type Installed struct {
	Commit string // the commit the release tag pointed at for the whole download
	SHA256 string // of the release tarball
	Jar    string // absolute path on the target
}

// Install downloads and unpacks the release. The log and exit file live beside
// InstallDir because the install command starts by deleting InstallDir.
func (a *App) Install(ctx context.Context) (Installed, error) {
	dir := path.Dir(a.cfg.InstallDir)
	logPath := path.Join(dir, "sqlancer-install.out")
	exitPath := path.Join(dir, "sqlancer-install.exit")
	cmd := fmt.Sprintf("mkdir -p %s && (%s) > %s 2>&1",
		shellQuote(dir), BuildInstallCommand(a.cfg), shellQuote(logPath))
	ictx, cancel := context.WithTimeout(ctx, a.cfg.InstallTimeout)
	defer cancel()
	code, err := a.runDetached(ictx, cmd, exitPath)
	out := a.read(ctx, logPath)
	if err != nil {
		return Installed{}, fmt.Errorf("sqlancer install: %w (log tail: %s)", err, tailLines(out, 5))
	}
	if code == exitTagMoved {
		return Installed{}, fmt.Errorf("sqlancer release %s moved during the download; rerun: %s",
			a.cfg.ReleaseTag, tailLines(out, 1))
	}
	if code != 0 {
		return Installed{}, fmt.Errorf("sqlancer install exited %d: %s", code, tailLines(out, 10))
	}
	lines := nonEmptyLines(out)
	if len(lines) < 3 {
		return Installed{}, fmt.Errorf("sqlancer install printed no commit, sha256 and jar: %q", out)
	}
	got := Installed{Commit: lines[len(lines)-3], SHA256: lines[len(lines)-2], Jar: lines[len(lines)-1]}
	if !isHex(got.Commit, 40) || !isHex(got.SHA256, 64) || !strings.HasSuffix(got.Jar, ".jar") {
		return Installed{}, fmt.Errorf("sqlancer install output is not commit, sha256, jar: %q", tailLines(out, 3))
	}
	return got, nil
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// Run executes one oracle to completion and classifies it. An error is a
// harness or environment problem; a finding is a Result, never an error.
//
// A run that could not be watched to its end (abandoned past its deadline,
// unwatchable, its process gone) is still read for reproducers: a thread
// stuck on a statement amp never answers must not discard the bug another
// thread already wrote down. With any on disk the run is a finding, and
// RunError says why it did not end on its own.
func (a *App) Run(ctx context.Context, s RunSpec) (Result, error) {
	rctx, cancel := context.WithTimeout(ctx, s.Timeout+exitGrace)
	defer cancel()
	code, runErr := a.runDetached(rctx, BuildRunCommand(s), s.ExitPath())
	lctx, lcancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer lcancel()
	findings, err := a.listFindings(lctx, s)
	if runErr != nil {
		if len(findings) > 0 {
			return Result{Verdict: VerdictFindings, ExitCode: -1, Findings: findings, RunError: runErr.Error()}, nil
		}
		return Result{ExitCode: -1}, fmt.Errorf("sqlancer %s: %w", s.Oracle, runErr)
	}
	if err != nil {
		return Result{}, fmt.Errorf("sqlancer %s: list findings: %w", s.Oracle, err)
	}
	res := Result{ExitCode: code, Findings: findings}
	switch {
	case len(findings) > 0:
		res.Verdict = VerdictFindings
	case code == 0:
		res.Verdict = VerdictClean
	default:
		// Non-zero with no reproducer: SQLancer never got going (no connection,
		// a bad flag, an OOM), so the run says nothing about amp.
		return res, fmt.Errorf("sqlancer %s exited %d with no reproducer log: %s",
			s.Oracle, code, tailLines(a.read(ctx, s.LogPath()), 10))
	}
	return res, nil
}

// Collect attaches the run's output and every reproducer, each capped.
func (a *App) Collect(ctx context.Context, s RunSpec, r Result, into apps.ArtifactSink) {
	prefix := "sqlancer/" + strings.ToLower(s.Oracle)
	a.collectFile(ctx, into, s.LogPath(), prefix+"/sqlancer.out")
	for _, f := range r.Findings {
		a.collectFile(ctx, into, path.Join(s.FindingsDir(), f), prefix+"/"+f)
	}
}

// listFindings returns the reproducer logs: every *.log under FindingsDir that
// is not a thread's -cur.log.
func (a *App) listFindings(ctx context.Context, s RunSpec) ([]string, error) {
	res, err := a.ex.Run(ctx, fmt.Sprintf("ls -1 %s 2>/dev/null; true", shellQuote(s.FindingsDir())))
	if err != nil {
		return nil, err
	}
	return FindingLogs(res.Stdout), nil
}

// FindingLogs filters an `ls -1` of logs/amp down to the reproducer logs.
func FindingLogs(ls string) []string {
	var out []string
	for _, f := range nonEmptyLines(ls) {
		if strings.HasSuffix(f, ".log") && !strings.HasSuffix(f, "-cur.log") {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// runDetached launches cmd detached and polls exitPath until it holds a code.
// Cancelling ctx kills the wrapper and its children best effort.
func (a *App) runDetached(ctx context.Context, cmd, exitPath string) (int, error) {
	res, err := a.ex.Run(ctx, BuildStartCommand(cmd, exitPath))
	if err != nil {
		return 0, fmt.Errorf("start: %w", err)
	}
	pid := strings.TrimSpace(res.Stdout)
	if res.ExitCode != 0 || pid == "" {
		return 0, fmt.Errorf("start exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stdout+res.Stderr))
	}
	probe := "cat " + shellQuote(exitPath) + " 2>/dev/null" +
		" || (kill -0 " + pid + " 2>/dev/null && echo RUNNING || echo GONE)"
	poll := a.cfg.PollInterval
	if poll <= 0 {
		poll = DefaultPollInterval
	}
	pollErrs := 0
	gone := false
	for {
		select {
		case <-ctx.Done():
			// Children first: killing the wrapper reparents the JVM and orphans it.
			kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			_, _ = a.ex.Run(kctx, "pkill -9 -P "+pid+" 2>/dev/null; kill -9 "+pid+" 2>/dev/null")
			cancel()
			return 0, fmt.Errorf("abandoned: %w", ctx.Err())
		case <-time.After(poll):
		}
		res, err := a.ex.Run(ctx, probe)
		if err != nil || res.ExitCode != 0 {
			pollErrs++
			if pollErrs >= maxConsecutivePollErrors {
				return 0, fmt.Errorf("unwatchable, %d consecutive poll failures, last: %v", pollErrs, err)
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
				return 0, fmt.Errorf("process (pid %s) disappeared without writing %s", pid, exitPath)
			}
			gone = true
		default:
			code, cerr := strconv.Atoi(out)
			if cerr != nil {
				return 0, fmt.Errorf("exit file %s holds %q, not an exit code", exitPath, out)
			}
			return code, nil
		}
	}
}

func (a *App) read(ctx context.Context, remote string) string {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	res, err := a.ex.Run(rctx, fmt.Sprintf("tail -c %d %s 2>/dev/null", a.maxBytes(), shellQuote(remote)))
	if err != nil {
		return ""
	}
	return res.Stdout
}

func (a *App) collectFile(ctx context.Context, into apps.ArtifactSink, remote, name string) {
	res, err := a.ex.Run(ctx, fmt.Sprintf("wc -c < %s 2>/dev/null", shellQuote(remote)))
	if err != nil || res.ExitCode != 0 {
		return
	}
	size, _ := strconv.ParseInt(strings.TrimSpace(res.Stdout), 10, 64)
	if size == 0 {
		return
	}
	body := a.read(ctx, remote)
	if size > int64(a.maxBytes()) {
		body = fmt.Sprintf("[ampqa: truncated — last %d of %d bytes]\n%s", a.maxBytes(), size, body)
	}
	if err := into.Add(name, "log", []byte(body)); err != nil {
		a.logf("sqlancer collect %s: %v", name, err)
	}
}

func (a *App) maxBytes() int {
	if a.cfg.CollectMaxBytes > 0 {
		return a.cfg.CollectMaxBytes
	}
	return DefaultCollectMaxBytes
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func tailLines(s string, n int) string {
	lines := nonEmptyLines(s)
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
