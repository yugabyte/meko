// Package yugabyte is the raw YugabyteDB-cluster App: it brings a cluster up by
// starting yb-master and yb-tserver directly over a set of RemoteExecutors
// (one per node), the yugabyted-free path — the stress framework's
// yugabyte_app.py equivalent narrowed to what ampqa needs to run a cluster as a
// TEST TARGET. Use this over apps/yugabyted when the test needs the explicit
// master/tserver control the wrapper hides.
//
// Layout (the standard, minimal RF=3 cluster): given N nodes and RF r, the
// first r nodes each run a yb-master AND a yb-tserver, and every node runs a
// yb-tserver. So a 3-node RF=3 cluster is 3 masters + 3 tservers co-located on
// the same 3 hosts — the canonical production shape. master_addresses is the
// r master hosts; tservers point at it via --tserver_master_addrs.
//
// Lifecycle: Start deploys the release (when configured), starts every master,
// waits for a master leader, then starts every tserver; Wait polls until YSQL
// answers and the masters report a leader; Status queries the masters; Stop
// halts tservers then masters (reverse of start); Destroy collects logs then
// stops and wipes the data dirs (collect-before-kill via apps.StopWithCollect);
// Collect pulls the master/tserver INFO/WARNING logs. It satisfies apps.App.
package yugabyte

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

// Defaults (also mirrored in configs/default.yaml under apps.yugabyte.*).
const (
	DefaultRF              = 3
	DefaultYSQLPort        = 5433
	DefaultMasterRPCPort   = 7100
	DefaultTServerRPCPort  = 9100
	DefaultMasterHTTPPort  = 7000
	DefaultTServerHTTPPort = 9000
	DefaultDataDir         = "/home/ec2-user/yb-data"
	DefaultReadyWait       = 10 * time.Minute

	ybUser     = "yugabyte"
	ybDatabase = "yugabyte"

	// postInstallScript runs bin/fips_install.sh (FIPS/OpenSSL config) on a
	// bare tarball deploy. yb-master/yb-tserver are native binaries with no
	// Python dependency (unlike apps/yugabyted's `yugabyted` launcher).
	postInstallScript  = "bin/post_install.sh"
	postInstallMarker  = ".ampqa_post_install_done"
	postInstallTimeout = 2 * time.Minute
)

// Node is one cluster member: a RemoteExecutor to run commands on it, and the
// address it advertises to peers (and the coordinator reaches YSQL at).
type Node struct {
	Exec      exec.RemoteExecutor
	Advertise string
}

// DeploySpec optionally delivers the yugabyte release to every node before
// start. The unpacked install must place bin/yb-master and bin/yb-tserver under
// Spec.InstallDir.
type DeploySpec struct {
	Artifact  artifact.Spec
	RemoteDir string
	Secret    string
}

// Spec configures a raw yugabyte cluster.
type Spec struct {
	// InstallDir is the yugabyte install root on each node (contains
	// bin/yb-master, bin/yb-tserver). "" → assume they are on PATH.
	InstallDir string
	// DataDir is --fs_data_dirs on each node; master writes yb-data/master/…,
	// tserver writes yb-data/tserver/… underneath, so one dir serves both.
	// "" → DefaultDataDir.
	DataDir string
	// RF is the replication factor: the first RF nodes run masters. 0 →
	// DefaultRF (clamped to the node count).
	RF int

	YSQLPort        int // 0 → DefaultYSQLPort
	MasterRPCPort   int // 0 → DefaultMasterRPCPort
	TServerRPCPort  int // 0 → DefaultTServerRPCPort
	MasterHTTPPort  int // 0 → DefaultMasterHTTPPort
	TServerHTTPPort int // 0 → DefaultTServerHTTPPort

	// MasterFlags / TServerFlags are extra --key=value gflags appended verbatim.
	MasterFlags  map[string]string
	TServerFlags map[string]string

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

// App is a raw yugabyte cluster driven over per-node RemoteExecutors.
type App struct {
	nodes    []Node
	masters  []Node
	spec     Spec
	deployer artifactDeployer
	waitPG   func(ctx context.Context, ci pg.ConnInfo, timeout time.Duration) error
	logf     func(format string, args ...any)
}

// Option customizes an App.
type Option func(*App)

// WithDeployer injects the artifact deployer used for Spec.Deploy.
func WithDeployer(d artifactDeployer) Option { return func(a *App) { a.deployer = d } }

// WithLogf injects the log sink (default log.Printf).
func WithLogf(logf func(string, ...any)) Option { return func(a *App) { a.logf = logf } }

// WithWaitPG injects the YSQL readiness probe (default pg.WaitReady). A caller
// routes through its own connection policy; tests use it to avoid a real DB.
func WithWaitPG(f func(context.Context, pg.ConnInfo, time.Duration) error) Option {
	return func(a *App) { a.waitPG = f }
}

// New builds a raw yugabyte App over nodes with spec. The first RF nodes run
// masters; every node runs a tserver.
func New(nodes []Node, spec Spec, opts ...Option) *App {
	a := &App{
		nodes:    nodes,
		spec:     spec,
		deployer: &artifact.Deployer{},
		waitPG:   pg.WaitReady,
		logf:     log.Printf,
	}
	rf := a.rf()
	if rf > 0 && rf <= len(nodes) {
		a.masters = nodes[:rf]
	} else {
		a.masters = nodes
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

func (a *App) rf() int {
	rf := a.spec.RF
	if rf <= 0 {
		rf = DefaultRF
	}
	if rf > len(a.nodes) {
		rf = len(a.nodes)
	}
	return rf
}

func (a *App) dataDir() string {
	if a.spec.DataDir != "" {
		return a.spec.DataDir
	}
	return DefaultDataDir
}

func portOr(v, def int) int {
	if v != 0 {
		return v
	}
	return def
}

func (a *App) ysqlPort() int    { return portOr(a.spec.YSQLPort, DefaultYSQLPort) }
func (a *App) masterRPC() int   { return portOr(a.spec.MasterRPCPort, DefaultMasterRPCPort) }
func (a *App) tserverRPC() int  { return portOr(a.spec.TServerRPCPort, DefaultTServerRPCPort) }
func (a *App) masterHTTP() int  { return portOr(a.spec.MasterHTTPPort, DefaultMasterHTTPPort) }
func (a *App) tserverHTTP() int { return portOr(a.spec.TServerHTTPPort, DefaultTServerHTTPPort) }

func (a *App) bin(name string) string {
	if a.spec.InstallDir != "" {
		return shellQuote(path.Join(a.spec.InstallDir, "bin", name))
	}
	return name
}

// masterAddresses is the comma-separated <host>:<rpc> list every master and
// tserver is told about.
func (a *App) masterAddresses() string {
	parts := make([]string, len(a.masters))
	for i, m := range a.masters {
		parts[i] = fmt.Sprintf("%s:%d", m.Advertise, a.masterRPC())
	}
	return strings.Join(parts, ",")
}

// ConnInfo is the YSQL target for the cluster (node 0 by default).
func (a *App) ConnInfo() pg.ConnInfo {
	host := ""
	if len(a.nodes) > 0 {
		host = a.nodes[0].Advertise
	}
	return pg.ConnInfo{Host: host, Port: a.ysqlPort(), User: ybUser, Database: ybDatabase}
}

// YSQLAddrs is every node's tserver YSQL host:port (every node runs a tserver),
// for use as a backing-store host-ports list.
func (a *App) YSQLAddrs() []string {
	addrs := make([]string, len(a.nodes))
	for i, n := range a.nodes {
		addrs[i] = fmt.Sprintf("%s:%d", n.Advertise, a.ysqlPort())
	}
	return addrs
}

// Start deploys the release (when configured), starts every master, waits for a
// master leader, then starts every tserver.
func (a *App) Start(ctx context.Context) error {
	if len(a.nodes) == 0 {
		return fmt.Errorf("yugabyte start: no nodes")
	}
	if a.spec.Deploy != nil {
		if err := a.deploy(ctx); err != nil {
			return err
		}
	}
	if err := a.runPostInstall(ctx); err != nil {
		return err
	}
	if err := a.prepareDirs(ctx); err != nil {
		return err
	}
	for _, m := range a.masters {
		if err := a.startMaster(ctx, m); err != nil {
			return fmt.Errorf("start master %s: %w", m.Advertise, err)
		}
	}
	if err := a.waitMastersHealthy(ctx); err != nil {
		return fmt.Errorf("masters never elected a leader: %w", err)
	}
	for _, n := range a.nodes {
		if err := a.startTServer(ctx, n); err != nil {
			return fmt.Errorf("start tserver %s: %w", n.Advertise, err)
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
// before any yb-master/yb-tserver binary is invoked. A marker file skips it on
// repeated Starts; a missing script (no InstallDir, or a release that lacks
// one) is logged and skipped rather than failing the start.
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
			a.log("yugabyte: no bin/post_install.sh on %s, skipping", n.Advertise)
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

func (a *App) prepareDirs(ctx context.Context) error {
	cmd := fmt.Sprintf("mkdir -p %s", shellQuote(a.dataDir()))
	for _, n := range a.nodes {
		if err := a.runOK(ctx, n.Exec, cmd); err != nil {
			return fmt.Errorf("prepare data dir on %s: %w", n.Advertise, err)
		}
	}
	return nil
}

func (a *App) startMaster(ctx context.Context, n Node) error {
	dir := a.dataDir()
	var b strings.Builder
	fmt.Fprintf(&b, "nohup %s", a.bin("yb-master"))
	fmt.Fprintf(&b, " --master_addresses=%s", a.masterAddresses())
	fmt.Fprintf(&b, " --rpc_bind_addresses=%s:%d", n.Advertise, a.masterRPC())
	fmt.Fprintf(&b, " --server_broadcast_addresses=%s:%d", n.Advertise, a.masterRPC())
	fmt.Fprintf(&b, " --webserver_port=%d", a.masterHTTP())
	fmt.Fprintf(&b, " --fs_data_dirs=%s", shellQuote(dir))
	fmt.Fprintf(&b, " --replication_factor=%d", a.rf())
	appendFlags(&b, a.spec.MasterFlags)
	fmt.Fprintf(&b, " >> %s 2>&1 &", shellQuote(path.Join(dir, "yb-master.out")))
	return a.runOK(ctx, n.Exec, b.String())
}

func (a *App) startTServer(ctx context.Context, n Node) error {
	dir := a.dataDir()
	var b strings.Builder
	fmt.Fprintf(&b, "nohup %s", a.bin("yb-tserver"))
	fmt.Fprintf(&b, " --tserver_master_addrs=%s", a.masterAddresses())
	fmt.Fprintf(&b, " --rpc_bind_addresses=%s:%d", n.Advertise, a.tserverRPC())
	fmt.Fprintf(&b, " --server_broadcast_addresses=%s:%d", n.Advertise, a.tserverRPC())
	fmt.Fprintf(&b, " --webserver_port=%d", a.tserverHTTP())
	fmt.Fprintf(&b, " --fs_data_dirs=%s", shellQuote(dir))
	fmt.Fprintf(&b, " --enable_ysql=true")
	fmt.Fprintf(&b, " --pgsql_proxy_bind_address=%s:%d", n.Advertise, a.ysqlPort())
	appendFlags(&b, a.spec.TServerFlags)
	fmt.Fprintf(&b, " >> %s 2>&1 &", shellQuote(path.Join(dir, "yb-tserver.out")))
	return a.runOK(ctx, n.Exec, b.String())
}

// waitMastersHealthy polls `yb-admin list_all_masters` on the first master
// until the output reports a LEADER, ctx is cancelled, or the deadline passes.
func (a *App) waitMastersHealthy(ctx context.Context) error {
	timeout := a.spec.ReadyTimeout
	if timeout <= 0 {
		timeout = DefaultReadyWait
	}
	deadline := time.Now().Add(timeout)
	wctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	var last error
	for {
		select {
		case <-wctx.Done():
			if last != nil {
				return fmt.Errorf("%w (last: %v)", wctx.Err(), last)
			}
			return wctx.Err()
		default:
		}
		out, err := a.listMasters(wctx)
		if err == nil && strings.Contains(out, "LEADER") {
			return nil
		}
		last = err
		select {
		case <-wctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
}

func (a *App) listMasters(ctx context.Context) (string, error) {
	if len(a.masters) == 0 {
		return "", fmt.Errorf("no masters")
	}
	cmd := fmt.Sprintf("%s --master_addresses %s list_all_masters", a.bin("yb-admin"), a.masterAddresses())
	res, err := a.masters[0].Exec.Run(ctx, cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return res.Stdout, fmt.Errorf("list_all_masters exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

// Wait blocks until YSQL answers on the cluster and the masters report a
// leader, or the deadline elapses.
func (a *App) Wait(ctx context.Context) error {
	timeout := a.spec.ReadyTimeout
	if timeout <= 0 {
		timeout = DefaultReadyWait
	}
	if err := a.waitPG(ctx, a.ConnInfo(), timeout); err != nil {
		return fmt.Errorf("yugabyte YSQL never became ready: %w", err)
	}
	if err := a.waitMastersHealthy(ctx); err != nil {
		return fmt.Errorf("yugabyte masters not healthy: %w", err)
	}
	return nil
}

// WaitReady satisfies apps.App.
func (a *App) WaitReady(ctx context.Context) error { return a.Wait(ctx) }

// Stop halts every tserver then every master (reverse of start). Best-effort
// and never fatal: the instances are destroyed by the host teardown right
// after anyway, so a stop failure (already dead, signalled, unreachable) is
// logged and swallowed, never returned.
func (a *App) Stop(ctx context.Context) error {
	// tservers first (all nodes), then masters — reverse node order within each.
	a.pkillAll(ctx, "yb-tserver")
	a.pkillMasters(ctx)
	return nil
}

func (a *App) pkillAll(ctx context.Context, proc string) {
	cmd := fmt.Sprintf("pkill -9 -f %s || true", shellQuote(selfSafePattern(proc)))
	for i := len(a.nodes) - 1; i >= 0; i-- {
		if err := a.runOK(ctx, a.nodes[i].Exec, cmd); err != nil {
			a.log("yugabyte: stop %s on %s failed (best-effort): %v", proc, a.nodes[i].Advertise, err)
		}
	}
}

func (a *App) pkillMasters(ctx context.Context) {
	cmd := fmt.Sprintf("pkill -9 -f %s || true", shellQuote(selfSafePattern("yb-master")))
	for i := len(a.masters) - 1; i >= 0; i-- {
		if err := a.runOK(ctx, a.masters[i].Exec, cmd); err != nil {
			a.log("yugabyte: stop yb-master on %s failed (best-effort): %v", a.masters[i].Advertise, err)
		}
	}
}

// selfSafePattern brackets proc's first byte ("yb-master" -> "[y]b-master")
// so `pkill -9 -f <pattern>` still matches a real yb-master/yb-tserver process
// (pkill -f treats the pattern as a regex, and "[y]" matches "y") but never
// matches pkill's own argv, which contains the literal bracketed string, not
// "yb-master" — without this, pkill matches and kills itself (SIGKILL, exit
// 137) whenever its own command line contains the process name it's hunting.
func selfSafePattern(proc string) string {
	if proc == "" {
		return proc
	}
	return "[" + proc[:1] + "]" + proc[1:]
}

// Destroy collects logs, stops the cluster, and wipes the data dir on every
// node. Collect runs before stop (apps.StopWithCollect) so logs are never lost.
func (a *App) Destroy(ctx context.Context, into apps.ArtifactSink) error {
	return apps.StopWithCollect(ctx, into, a.Collect, a.stopAndWipe)
}

func (a *App) stopAndWipe(ctx context.Context) error {
	stopErr := a.Stop(ctx)
	wipe := fmt.Sprintf("rm -rf %s", shellQuote(a.dataDir()))
	for _, n := range a.nodes {
		if _, err := n.Exec.Run(ctx, wipe); err != nil {
			a.log("yugabyte: wipe data dir on %s failed (best-effort): %v", n.Advertise, err)
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
// remove the data dir.
func (a *App) Close(ctx context.Context) error { return a.Stop(ctx) }

// Collect pulls the master and tserver INFO/WARNING logs off every node into
// the sink. Best-effort: unreadable files are logged and skipped, not fatal.
func (a *App) Collect(ctx context.Context, into apps.ArtifactSink) error {
	dir := a.dataDir()
	masterLogs := path.Join(dir, "yb-data", "master", "logs")
	tserverLogs := path.Join(dir, "yb-data", "tserver", "logs")

	for _, m := range a.masters {
		for _, f := range []string{"yb-master.INFO", "yb-master.WARNING"} {
			if err := a.collectFile(ctx, into, m, path.Join(masterLogs, f), f); err != nil {
				return err
			}
		}
	}
	for _, n := range a.nodes {
		for _, f := range []string{"yb-tserver.INFO", "yb-tserver.WARNING"} {
			if err := a.collectFile(ctx, into, n, path.Join(tserverLogs, f), f); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *App) collectFile(ctx context.Context, into apps.ArtifactSink, n Node, remote, name string) error {
	res, err := n.Exec.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null", shellQuote(remote)))
	if err != nil {
		a.log("yugabyte collect: %s on %s: %v", name, n.Advertise, err)
		return nil
	}
	if strings.TrimSpace(res.Stdout) == "" {
		return nil
	}
	return into.Add(n.Advertise+"/"+name, "log", []byte(res.Stdout))
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

// appendFlags appends extra --key=value gflags in a deterministic order.
func appendFlags(b *strings.Builder, flags map[string]string) {
	for _, k := range sortedKeys(flags) {
		fmt.Fprintf(b, " --%s=%s", k, flags[k])
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}

// shellQuote wraps s in single quotes, escaping embedded single quotes, so it
// is safe as one argument in a remote /bin/sh command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
