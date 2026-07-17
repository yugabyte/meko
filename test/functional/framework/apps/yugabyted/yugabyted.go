// Package yugabyted is the yugabyted-cluster App: it brings a YugabyteDB
// cluster up over a set of RemoteExecutors (one per node) using the `yugabyted`
// CLI — the yugabyted-without-YBA path, the stress framework's yugabyted_app
// equivalent narrowed to what ampqa needs to run a cluster as a TEST TARGET.
//
// Lifecycle: Start deploys the release and runs post_install (when
// configured), ensures every node has a Python 3.7+ interpreter (yugabyted's
// launcher needs one; see ensurePython), then bootstraps node 0 (`yugabyted
// start --advertise_address … --base_dir … --background`) and joins nodes
// 1..N (`--join <node0>`); Wait
// polls until YSQL answers and `yugabyted status` is healthy; Status/Stop query
// and halt; Destroy stops and wipes the base dir (collect-before-kill enforced
// via apps.StopWithCollect); Collect pulls the yugabyted and master/tserver
// logs. When a Spec.Deploy is set the yugabyte release tarball is delivered to
// every node via artifact/ before start; otherwise an existing install (or
// yugabyted on PATH) is assumed.
//
// It satisfies apps.App (WaitReady/Signal/Collect/Close) so tasks, tests, and
// the env/universe provider drive it through the same uniform lifecycle as
// every other app.
package yugabyted

import (
	"context"
	"fmt"
	"log"
	"path"
	"strings"
	"time"

	"github.com/yugabyte/yb-amp/test/functional/framework/apps"
	"github.com/yugabyte/yb-amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/yb-amp/test/functional/framework/artifact"
	"github.com/yugabyte/yb-amp/test/functional/framework/exec"
)

// Defaults.
const (
	DefaultYSQLPort = 5433
	// DefaultBaseDir must be writable by the ssh user yugabyted actually runs
	// as (ec2-user, not root): yugabyted's own default base_dir resolves under
	// /root regardless of the invoking user, and handle_config_files's
	// os.makedirs(conf_dir) then fails with PermissionError. Override via
	// Spec.BaseDir (functional: functional.cluster.base_dir).
	DefaultBaseDir   = "/home/ec2-user/yugabyte-data"
	DefaultReadyWait = 5 * time.Minute
	ybUser           = "yugabyte"
	ybDatabase       = "yugabyte"

	// postInstallScript runs bin/fips_install.sh (FIPS/OpenSSL config) on a
	// bare tarball deploy. It does not provision a Python interpreter — see
	// ensurePython, which handles that separately.
	postInstallScript  = "bin/post_install.sh"
	postInstallMarker  = ".ampqa_post_install_done"
	postInstallTimeout = 2 * time.Minute

	// pythonProbeCmd checks whether python3 can import the stdlib dataclasses
	// module (Python 3.7+). yugabyted's launcher has a bare
	// #!/usr/bin/env python3 shebang and no bundled interpreter; on a
	// mislabeled el8 AMI whose /usr/bin/python3 is platform-python 3.6, it
	// dies with ModuleNotFoundError: dataclasses.
	pythonProbeCmd = "python3 -c 'import dataclasses'"

	// DefaultEnsurePython is ensurePython's default enablement (mirrored in
	// configs/default.yaml as apps.yugabyted.ensure_python).
	DefaultEnsurePython = true

	// DefaultPythonSetupCmd installs python39 via dnf and points the python3
	// alternatives entry at it; only run when pythonProbeCmd fails. Config-
	// overridable (apps.yugabyted.python_setup_cmd) for a non-dnf distro.
	DefaultPythonSetupCmd = `python3 -c 'import dataclasses' 2>/dev/null || { sudo dnf install -y python39 && sudo alternatives --set python3 /usr/bin/python3.9 2>/dev/null || sudo alternatives --install /usr/bin/python3 python3 /usr/bin/python3.9 60; }`

	pythonSetupTimeout = 3 * time.Minute
)

// Node is one cluster member: a RemoteExecutor to run commands on it, and the
// address it advertises to peers (and the coordinator reaches YSQL at).
type Node struct {
	Exec      exec.RemoteExecutor
	Advertise string
}

// DeploySpec optionally delivers the yugabyte release to every node before
// start. The unpacked install must place bin/yugabyted under Spec.InstallDir.
type DeploySpec struct {
	Artifact  artifact.Spec
	RemoteDir string
	Secret    string
}

// Spec configures a yugabyted cluster.
type Spec struct {
	// InstallDir is the yugabyte install root on each node (contains
	// bin/yugabyted). "" → assume `yugabyted` is on PATH.
	InstallDir string
	BaseDir    string // --base_dir; "" → DefaultBaseDir
	DataDir    string // --data_dir; "" → omitted (yugabyted default under base_dir)
	RF         int    // replication factor → --fault_tolerance; <=1 → none
	YSQLPort   int    // libpq port exposed in ConnInfo; 0 → 5433
	// YSQLHost overrides the host used in ConnInfo. "" → nodes[0].Advertise
	// (the coordinator must be able to reach it).
	YSQLHost string
	// Deploy, when set, delivers the release tarball to every node before start.
	Deploy *DeploySpec
	// ReadyTimeout bounds Wait. 0 → DefaultReadyWait.
	ReadyTimeout time.Duration
}

// artifactDeployer is the subset of *artifact.Deployer the app uses; injectable
// so tests assert the deploy path without a real fetch/SSH.
type artifactDeployer interface {
	Deploy(ctx context.Context, execs []exec.RemoteExecutor, spec artifact.Spec, opts artifact.DeployOptions) error
}

// App is a yugabyted cluster driven over per-node RemoteExecutors.
type App struct {
	nodes          []Node
	spec           Spec
	deployer       artifactDeployer
	waitPG         func(ctx context.Context, ci pg.ConnInfo, timeout time.Duration) error
	logf           func(format string, args ...any)
	pythonEnsure   bool // gates the ensurePython step; see WithEnsurePython
	pythonSetupCmd string
}

// Option customizes an App.
type Option func(*App)

// WithDeployer injects the artifact deployer used for Spec.Deploy.
func WithDeployer(d artifactDeployer) Option { return func(a *App) { a.deployer = d } }

// WithLogf injects the log sink (default log.Printf).
func WithLogf(logf func(string, ...any)) Option { return func(a *App) { a.logf = logf } }

// WithWaitPG injects the YSQL readiness probe (default pg.WaitReady). The
// env/universe provider uses it to route through its own connection policy;
// tests use it as a seam to avoid a real database.
func WithWaitPG(f func(context.Context, pg.ConnInfo, time.Duration) error) Option {
	return func(a *App) { a.waitPG = f }
}

// WithEnsurePython overrides whether Start ensures a Python 3.7+ interpreter
// on every node before invoking yugabyted (default DefaultEnsurePython;
// config: apps.yugabyted.ensure_python).
func WithEnsurePython(enabled bool) Option { return func(a *App) { a.pythonEnsure = enabled } }

// WithPythonSetupCmd overrides the install command ensurePython falls back to
// when pythonProbeCmd fails (default DefaultPythonSetupCmd; config:
// apps.yugabyted.python_setup_cmd).
func WithPythonSetupCmd(cmd string) Option { return func(a *App) { a.pythonSetupCmd = cmd } }

// New builds a yugabyted App over nodes with spec. nodes[0] is the bootstrap
// node the rest join.
func New(nodes []Node, spec Spec, opts ...Option) *App {
	a := &App{
		nodes:          nodes,
		spec:           spec,
		deployer:       &artifact.Deployer{},
		waitPG:         pg.WaitReady,
		logf:           log.Printf,
		pythonEnsure:   DefaultEnsurePython,
		pythonSetupCmd: DefaultPythonSetupCmd,
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

func (a *App) baseDir() string {
	if a.spec.BaseDir != "" {
		return a.spec.BaseDir
	}
	return DefaultBaseDir
}

func (a *App) ysqlPort() int {
	if a.spec.YSQLPort != 0 {
		return a.spec.YSQLPort
	}
	return DefaultYSQLPort
}

// bin is the yugabyted binary invocation, install-dir-qualified when set.
func (a *App) bin() string {
	if a.spec.InstallDir != "" {
		return shellQuote(path.Join(a.spec.InstallDir, "bin", "yugabyted"))
	}
	return "yugabyted"
}

// ConnInfo is the YSQL target for the cluster (node 0 by default).
func (a *App) ConnInfo() pg.ConnInfo {
	host := a.spec.YSQLHost
	if host == "" && len(a.nodes) > 0 {
		host = a.nodes[0].Advertise
	}
	return pg.ConnInfo{Host: host, Port: a.ysqlPort(), User: ybUser, Database: ybDatabase}
}

// YSQLAddrs is every node's YSQL host:port (each yugabyted node serves YSQL),
// for use as a backing-store host-ports list.
func (a *App) YSQLAddrs() []string {
	addrs := make([]string, len(a.nodes))
	for i, n := range a.nodes {
		addrs[i] = fmt.Sprintf("%s:%d", n.Advertise, a.ysqlPort())
	}
	return addrs
}

// Start deploys the release (when configured), bootstraps node 0, and joins the
// remaining nodes.
func (a *App) Start(ctx context.Context) error {
	if len(a.nodes) == 0 {
		return fmt.Errorf("yugabyted start: no nodes")
	}
	if a.spec.Deploy != nil {
		if err := a.deploy(ctx); err != nil {
			return err
		}
	}
	if err := a.runPostInstall(ctx); err != nil {
		return err
	}
	if err := a.ensurePython(ctx); err != nil {
		return err
	}
	first := a.nodes[0].Advertise
	if err := a.startNode(ctx, a.nodes[0], ""); err != nil {
		return fmt.Errorf("bootstrap node %s: %w", first, err)
	}
	for i := 1; i < len(a.nodes); i++ {
		if err := a.startNode(ctx, a.nodes[i], first); err != nil {
			return fmt.Errorf("join node %s: %w", a.nodes[i].Advertise, err)
		}
	}
	return nil
}

func (a *App) deploy(ctx context.Context) error {
	execs := make([]exec.RemoteExecutor, len(a.nodes))
	for i, n := range a.nodes {
		execs[i] = n.Exec
	}
	if err := a.deployer.Deploy(ctx, execs, a.spec.Deploy.Artifact, artifact.DeployOptions{
		RemoteDir: a.spec.Deploy.RemoteDir,
		Secret:    a.spec.Deploy.Secret,
	}); err != nil {
		return fmt.Errorf("deploy yugabyte release: %w", err)
	}
	return nil
}

// runPostInstall runs bin/post_install.sh on every node once per InstallDir,
// before any yugabyted binary is invoked. A marker file skips it on repeated
// Starts; a missing script (no InstallDir, or a release that lacks one) is
// logged and skipped rather than failing the start.
func (a *App) runPostInstall(ctx context.Context) error {
	if a.spec.InstallDir == "" {
		return nil
	}
	marker := path.Join(a.spec.InstallDir, postInstallMarker)
	script := path.Join(a.spec.InstallDir, postInstallScript)
	for _, n := range a.nodes {
		done, err := a.probeFile(ctx, n.Exec, marker)
		if err != nil {
			return fmt.Errorf("post_install: probe marker on %s: %w", n.Advertise, err)
		}
		if done {
			continue
		}
		exists, err := a.probeFile(ctx, n.Exec, script)
		if err != nil {
			return fmt.Errorf("post_install: probe script on %s: %w", n.Advertise, err)
		}
		if !exists {
			a.log("yugabyted: no bin/post_install.sh on %s, skipping", n.Advertise)
			continue
		}
		if err := a.execPostInstall(ctx, n); err != nil {
			return err
		}
		if err := a.runOK(ctx, n.Exec, fmt.Sprintf("touch %s", shellQuote(marker))); err != nil {
			return fmt.Errorf("post_install: mark done on %s: %w", n.Advertise, err)
		}
	}
	return nil
}

// execPostInstall runs post_install.sh on n under a generous per-node
// timeout, folding stdout/stderr into the error on failure for diagnosis.
func (a *App) execPostInstall(ctx context.Context, n Node) error {
	pctx, cancel := context.WithTimeout(ctx, postInstallTimeout)
	defer cancel()
	cmd := fmt.Sprintf("cd %s && ./bin/post_install.sh", shellQuote(a.spec.InstallDir))
	res, err := n.Exec.Run(pctx, cmd)
	if err != nil {
		return fmt.Errorf("post_install: run on %s: %w", n.Advertise, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("post_install: %s exit %d: stdout=%s stderr=%s",
			n.Advertise, res.ExitCode, strings.TrimSpace(res.Stdout), strings.TrimSpace(res.Stderr))
	}
	return nil
}

// probeFile reports whether p exists on ex via `test -f`.
func (a *App) probeFile(ctx context.Context, ex exec.RemoteExecutor, p string) (bool, error) {
	res, err := ex.Run(ctx, fmt.Sprintf("test -f %s", shellQuote(p)))
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

// ensurePython makes sure every node has a python3 that can import the
// stdlib dataclasses module (Python 3.7+) before any bin/yugabyted command
// runs. No-op when disabled via WithEnsurePython.
func (a *App) ensurePython(ctx context.Context) error {
	if !a.pythonEnsure {
		return nil
	}
	for _, n := range a.nodes {
		if err := a.ensurePythonOnNode(ctx, n); err != nil {
			return err
		}
	}
	return nil
}

// ensurePythonOnNode probes n for a working python3, and on failure runs
// pythonSetupCmd and re-verifies. A probe success (already-good interpreter)
// is a single fast command; the setup command only runs when needed.
func (a *App) ensurePythonOnNode(ctx context.Context, n Node) error {
	pctx, cancel := context.WithTimeout(ctx, pythonSetupTimeout)
	defer cancel()
	ok, err := a.probeDataclasses(pctx, n.Exec)
	if err != nil {
		return fmt.Errorf("ensure python: probe on %s: %w", n.Advertise, err)
	}
	if ok {
		return nil
	}
	res, err := n.Exec.Run(pctx, a.pythonSetupCmd)
	if err != nil {
		return fmt.Errorf("ensure python: setup on %s: %w", n.Advertise, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("ensure python: setup on %s exit %d: stdout=%s stderr=%s",
			n.Advertise, res.ExitCode, strings.TrimSpace(res.Stdout), strings.TrimSpace(res.Stderr))
	}
	ok, err = a.probeDataclasses(pctx, n.Exec)
	if err != nil {
		return fmt.Errorf("ensure python: re-verify on %s: %w", n.Advertise, err)
	}
	if !ok {
		return fmt.Errorf("ensure python: dataclasses still unimportable on %s after setup: stdout=%s stderr=%s",
			n.Advertise, strings.TrimSpace(res.Stdout), strings.TrimSpace(res.Stderr))
	}
	return nil
}

// probeDataclasses reports whether ex's python3 can `import dataclasses`.
func (a *App) probeDataclasses(ctx context.Context, ex exec.RemoteExecutor) (bool, error) {
	res, err := ex.Run(ctx, pythonProbeCmd)
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

// startNode runs `yugabyted start` on n, joining joinHost when non-empty.
func (a *App) startNode(ctx context.Context, n Node, joinHost string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s start --advertise_address=%s --base_dir=%s --background=true",
		a.bin(), shellQuote(n.Advertise), shellQuote(a.baseDir()))
	if joinHost != "" {
		fmt.Fprintf(&b, " --join=%s", shellQuote(joinHost))
	}
	if a.spec.DataDir != "" {
		fmt.Fprintf(&b, " --data_dir=%s", shellQuote(a.spec.DataDir))
	}
	if a.spec.RF > 1 {
		fmt.Fprintf(&b, " --fault_tolerance=%s", faultTolerance(a.spec.RF))
	}
	return a.runOK(ctx, n.Exec, b.String())
}

// Wait blocks until YSQL answers on the cluster and node 0 reports a running
// status, or the deadline elapses.
func (a *App) Wait(ctx context.Context) error {
	timeout := a.spec.ReadyTimeout
	if timeout <= 0 {
		timeout = DefaultReadyWait
	}
	if err := a.waitPG(ctx, a.ConnInfo(), timeout); err != nil {
		return fmt.Errorf("yugabyted YSQL never became ready: %w", err)
	}
	if _, err := a.Status(ctx); err != nil {
		return fmt.Errorf("yugabyted status not healthy: %w", err)
	}
	return nil
}

// WaitReady satisfies apps.App.
func (a *App) WaitReady(ctx context.Context) error { return a.Wait(ctx) }

// Status returns node 0's `yugabyted status` output.
func (a *App) Status(ctx context.Context) (string, error) {
	if len(a.nodes) == 0 {
		return "", fmt.Errorf("yugabyted status: no nodes")
	}
	cmd := fmt.Sprintf("%s status --base_dir=%s", a.bin(), shellQuote(a.baseDir()))
	res, err := a.nodes[0].Exec.Run(ctx, cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return res.Stdout, fmt.Errorf("status exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

// Stop halts yugabyted on every node (reverse order — followers before the
// bootstrap node). Best-effort: the first error is returned after all attempts.
func (a *App) Stop(ctx context.Context) error {
	cmd := fmt.Sprintf("%s stop --base_dir=%s", a.bin(), shellQuote(a.baseDir()))
	var firstErr error
	for i := len(a.nodes) - 1; i >= 0; i-- {
		if err := a.runOK(ctx, a.nodes[i].Exec, cmd); err != nil {
			a.log("yugabyted: stop node %s failed (best-effort): %v", a.nodes[i].Advertise, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Destroy collects logs, stops the cluster, and wipes the base dir on every
// node. Collect runs before stop (apps.StopWithCollect) so logs are never lost
// to the teardown.
func (a *App) Destroy(ctx context.Context, into apps.ArtifactSink) error {
	return apps.StopWithCollect(ctx, into, a.Collect, a.stopAndWipe)
}

func (a *App) stopAndWipe(ctx context.Context) error {
	stopErr := a.Stop(ctx)
	wipe := fmt.Sprintf("rm -rf %s", shellQuote(a.baseDir()))
	for _, n := range a.nodes {
		if _, err := n.Exec.Run(ctx, wipe); err != nil {
			a.log("yugabyted: wipe base dir on %s failed (best-effort): %v", n.Advertise, err)
		}
	}
	return stopErr
}

// Signal maps a lifecycle signal onto the cluster: Stop and Kill both halt it.
func (a *App) Signal(ctx context.Context, sig apps.AppSignal) error {
	switch sig {
	case apps.SignalStop, apps.SignalKill:
		return a.Stop(ctx)
	default:
		return nil
	}
}

// Close stops the cluster (best-effort); it does not wipe. Use Destroy to also
// remove the base dir.
func (a *App) Close(ctx context.Context) error { return a.Stop(ctx) }

// Collect pulls the yugabyted and master/tserver logs off every node into the
// sink. Best-effort: unreadable files are logged and skipped, not fatal.
func (a *App) Collect(ctx context.Context, into apps.ArtifactSink) error {
	base := a.baseDir()
	files := []struct{ remote, name string }{
		{path.Join(base, "logs", "yugabyted.log"), "yugabyted.log"},
		{path.Join(base, "data", "yb-data", "master", "logs", "yb-master.INFO"), "yb-master.INFO"},
		{path.Join(base, "data", "yb-data", "tserver", "logs", "yb-tserver.INFO"), "yb-tserver.INFO"},
	}
	for _, n := range a.nodes {
		for _, f := range files {
			res, err := n.Exec.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null", shellQuote(f.remote)))
			if err != nil {
				a.log("yugabyted collect: %s on %s: %v", f.name, n.Advertise, err)
				continue
			}
			if strings.TrimSpace(res.Stdout) == "" {
				continue
			}
			if err := into.Add(n.Advertise+"/"+f.name, "log", []byte(res.Stdout)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *App) runOK(ctx context.Context, ex exec.RemoteExecutor, cmd string) error {
	res, err := ex.Run(ctx, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("command exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// faultTolerance maps an RF to yugabyted's --fault_tolerance argument. RF 3
// tolerates one node loss (zone), RF 5 two; anything else falls back to "none".
func faultTolerance(rf int) string {
	switch {
	case rf >= 5:
		return "zone"
	case rf >= 3:
		return "zone"
	default:
		return "none"
	}
}

// shellQuote wraps s in single quotes, escaping embedded single quotes, so it
// is safe as one argument in a remote /bin/sh command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
