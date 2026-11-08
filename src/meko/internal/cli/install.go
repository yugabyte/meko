package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yugabyte/meko/src/meko/internal/agents"
	"github.com/yugabyte/meko/src/meko/internal/callhome"
	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/extensions"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/service"
	"github.com/yugabyte/meko/src/meko/internal/spin"
	"github.com/yugabyte/meko/src/meko/internal/store"
	"github.com/yugabyte/meko/src/meko/internal/update"
	"github.com/yugabyte/meko/src/meko/internal/version"
)

// newReleaseClient reads a repo's GitHub releases; tests replace it.
var newReleaseClient = update.NewClient

// releaseClient is the client for [update]'s repo, checking each release's signature as
// require_signature says; notice is told of a release staged without that check.
func releaseClient(cfg config.Update, notice func(string)) *update.Client {
	c := newReleaseClient(cfg.Repo)
	c.AllowUnsigned, c.Notice = !cfg.RequireSignature, notice
	return c
}

// unsignedNotices keeps the notices of releases staged without a signature check, to print as
// warning rows once the steps (and their dots) are done.
type unsignedNotices struct {
	mu    sync.Mutex
	lines []string
}

func (n *unsignedNotices) add(s string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lines = append(n.lines, s)
}

// print prints the notices kept so far on w, as warning rows, and forgets them.
func (n *unsignedNotices) print(w io.Writer) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, l := range n.lines {
		spin.Row(w, spin.Warn, spin.Color(w), "Release", l)
	}
	n.lines = nil
}

// installCommand installs Meko's newest release, as the README and docs/install.md give it.
const installCommand = "curl -fsSL https://mekodata.ai/install.sh | sh"

// runningRelease finds the release install the running binary belongs to; tests replace it.
var runningRelease = update.Running

// runInstall sets this meko up: as a release install, app/current and the meko command point at
// it (and, asked, the shell's profile puts the command on the PATH); then the client token, the web
// listener's access, and the login service, which it starts once the port is known to be free (or
// moved to a free one: pickPort). It installs Meko alone: extensions are meko extension install's,
// which the picker at the end of a first install runs (pickExtension). Run by install.sh, it reads
// the MEKO_INSTALL_* variables the installer sets (installRun).
//
// A failure ends one way (installing.fail), and leaves either nothing written, or a setup that
// running the installer again finishes: app/setup-pending says which version's setup didn't finish
// until its server answers. An upgrade with a service goes on probation as an automatic update does
// (update.App.Switch), so a new version that can't start is rolled back, data and all.
func runInstall(l paths.Layout, noService bool, access string, noUI bool, stdout, stderr io.Writer) int {
	r := newReport(stdout)
	if access != "" && access != config.AccessLocal && access != config.AccessRemote {
		return fail(stderr, fmt.Errorf("--access takes local or remote, not %q", access))
	}
	run := installRun(os.Getenv)
	app, ver, release := runningRelease()
	in := &installing{l: l, r: r, run: run, release: release, version: version.Version, service: "none"}
	if release {
		in.version = ver
	}
	failed := func(label, step string, err error) int {
		return in.fail(installFailure{label: label, step: step, kind: callhome.KindError, err: err})
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return failed("Install", callhome.StepChecks, err)
	}
	exe, err := os.Executable()
	if err != nil {
		return failed("Install", callhome.StepChecks, err)
	}
	// The checks come before anything is written, so a failure leaves nothing half-done.
	var m service.Manager
	if !noService {
		if m, err = newManager(); err != nil {
			return in.fail(installFailure{label: "Service", step: callhome.StepChecks,
				kind: callhome.KindServiceManager, err: err})
		}
		in.service = m.Kind()
		if err = m.Preflight(); err != nil {
			return in.fail(installFailure{label: "Service", step: callhome.StepChecks,
				kind: callhome.KindServiceManager, err: err})
		}
	}
	if f := pickPort(l, m, home, r); f != nil {
		return in.fail(*f)
	}
	probation := false
	if release {
		// An upgrade with a service is on probation until it has run a while: a new version that
		// keeps failing to start is rolled back, its database with it (serve, update.App.Started).
		cur := app.Current()
		if !noService && cur != "" && cur != ver {
			if _, serr := os.Stat(app.Exe(cur)); serr == nil {
				in.from = cur
			}
		}
		// Finishing a setup that went on probation and didn't answer in time: the probation stays,
		// so serve can still roll back to the version before it.
		keep := false
		if !noService && cur == ver && app.SetupPending() == ver {
			if p, perr := app.ReadPending(); perr == nil && p != nil && p.To == ver {
				if _, serr := os.Stat(app.Exe(p.From)); serr == nil {
					in.from, keep, probation = p.From, true, true
				}
			}
		}
		if err = app.MarkSetup(ver, run.first); err != nil {
			return failed("Install", callhome.StepInstall, err)
		}
		in.written = true
		switch {
		case keep:
		case in.from != "":
			schema, _, perr := store.Peek(context.Background(), l.DB)
			if perr != nil {
				schema = 0 // unreadable: a rollback can't put a copy back, but still runs the old version
			}
			err = app.Switch(in.from, ver, schema)
			probation = err == nil
		default:
			// Installing by hand supersedes an update on probation.
			if err = app.Repoint(ver); err == nil {
				err = app.ClearPending()
			}
		}
		if err != nil {
			return failed("Install", callhome.StepInstall, err)
		}
		var link string
		if link, err = app.LinkCommand(home); err != nil {
			return failed("Install", callhome.StepInstall, err)
		}
		if _, err = app.LinkCommandAs(home, progMekoCLI); err != nil {
			return failed("Install", callhome.StepInstall, err)
		}
		exe = app.CurrentExe()
		installRow(r, filepath.Dir(link), home, run.ask)
	} else {
		fmt.Fprintf(stdout, "Setting up this build of meko (%s). It isn't from a release, "+
			"so it doesn't update itself\n", exe)
	}
	kept := paths.LooksLikeDataDir(l.Root)
	in.written = true
	if err = l.Ensure(); err != nil {
		return failed("Data", callhome.StepData, err)
	}
	cfg, err := config.Load(l.Config, os.Getenv, config.Overrides{})
	if err != nil {
		return failed("Data", callhome.StepData, err)
	}
	ct, err := clientTokens(l, cfg)
	if err != nil {
		return failed("Data", callhome.StepData, err)
	}
	if _, err = ct.Ensure(); err != nil {
		return failed("Data", callhome.StepData, err)
	}
	dataNote := "kept"
	if !kept {
		dataNote = map[string]string{"keychain": "secrets in your login Keychain",
			"file": "secrets in it, readable only by you"}[ct.Store.Kind()]
	}
	r.step(stepOK, "Data", spin.Note(homeRel(l.Root, home), dataNote, r.color))
	if err = setWebAccess(l, access, noUI, home); err != nil {
		return failed("Web UI", callhome.StepData, err)
	}
	if noService {
		if release {
			if err = app.ClearSetup(); err != nil {
				return failed("Install", callhome.StepInstall, err)
			}
		}
		if noUI {
			r.step(stepOK, "Web UI", fmt.Sprintf("%s, in %s", webOffNote, homeRel(config.WebFile(l.Config), home)))
		}
		r.step(stepWarn, "Service", "none (--no-service): start Meko with meko start")
		r.next([][2]string{
			{"meko start", "start Meko"},
			{"meko status", "check on Meko at any time"},
		})
		startFirstCommands(l, run)
		return exitOK
	}
	if other := m.DataDir(); m.Installed() && other != "" && other != l.Root {
		fmt.Fprintf(stdout, "The service ran the data folder %s; it now runs %s\n", other, l.Root)
	}
	// A server run by hand holds the data folder, and the service's own couldn't start.
	if up, _ := running(l.Lock); up && serviceOf(l) == nil {
		if code := stopMeko(l, stdout, stderr); code != exitOK {
			return failed("Server", callhome.StepService, errors.New("the Meko server started by hand on this "+
				"data folder didn't stop: stop it with meko stop"))
		}
	}
	host, _ := os.Hostname()
	pid := currentPID(l)
	logFrom := fileSize(l.ServiceLog)
	began := time.Now()
	if err = m.Install(service.Spec{Exe: exe, DataDir: l.Root, ServiceLog: l.ServiceLog, Host: host}); err != nil {
		return failed("Service", callhome.StepService, err)
	}
	r.step(stepOK, "Service", "starts at login, restarts on failure"+spin.Dim(" · stop it with ", r.color)+
		spin.Blue("meko stop", r.color))
	if sd, ok := m.(*service.Systemd); ok {
		if ok, detail, note := sd.Linger(); ok {
			r.step(stepOK, "Boot", detail)
		} else {
			r.step(stepWarn, "Boot", detail+". "+note)
		}
	}
	starting := spin.Quiet(stdout, "Server", "starting")
	w := waitStarted(l, serviceStartTimeout, pid, logFrom, !probation)
	switch {
	case w.ok && in.from != "" && w.st.Version == in.from:
		// The new version failed its starts, and serve put the old one back.
		reason := "it kept failing to start"
		if w.errLine != "" {
			reason = w.errLine
		}
		starting.End(stepFail, fmt.Sprintf("meko %s didn't start: %s", ver, reason))
		r.step(stepWarn, "Rolled back", spin.Note(fmt.Sprintf("meko %s is running again", in.from),
			"with its data as it was", r.color))
		_ = app.ClearSetup()
		fmt.Fprintf(stdout, "\n    %s%s\n", spin.Dim("More                 ", r.color),
			spin.Blue("meko logs --service", r.color))
		return in.fail(installFailure{step: callhome.StepServer, kind: callhome.KindRolledBack, printed: true,
			closing: fmt.Sprintf("Meko %s is installed and running", in.from)})
	case !w.ok:
		reason, kind := startReason(w, serviceStartTimeout)
		starting.End(stepFail, reason)
		// The service would start it again every few seconds, to fail the same way. An upgrade on
		// probation is left to serve, which rolls it back at its next start.
		stopped := !probation && m.Stop() == nil
		startFailed(r, w, m, stopped)
		return in.fail(installFailure{step: callhome.StepServer, kind: kind, printed: true})
	}
	if release {
		if err = app.ClearSetup(); err != nil {
			starting.Clear()
			return failed("Install", callhome.StepInstall, err)
		}
	}
	verb := "started"
	if pid != 0 || run.from != "" {
		verb = "restarted"
	}
	starting.Done(verb + " in " + seconds(time.Since(began)))
	o := runningOptions{kind: nextInstall, verb: "installed", since: run.started}
	if run.from != "" {
		o.verb = "upgraded"
	}
	if run.first {
		o.pick = pickExtension(l, run.ask, stdout, stderr)
	}
	printRunning(l, o, stdout)
	startFirstCommands(l, run)
	return exitOK
}

// startFirstCommands begins the list of the first commands after a first install, for the usage
// stats, once the install is done: what it ran itself (the picker's extension install, and the
// meko token --json that extension runs) isn't on it.
func startFirstCommands(l paths.Layout, run installRunInfo) {
	if run.first {
		_ = callhome.StartFirstCommands(l.FirstCommands, version.Version, time.Now())
	}
}

// setWebAccess writes how browsers reach Meko (its access) to web.toml (config.AddWeb) when neither it nor
// config.toml sets one: with access, or, unless a Kode is on this computer (whose own access Meko
// takes over when it hands over its port), with the default Kode had: remote over SSH or on Linux
// with no display, local otherwise. The summary's Web UI link row says which. With noUI it turns
// the web UI off there too (config.SetWebEnabled), which the summary's Web UI row says.
func setWebAccess(l paths.Layout, access string, noUI bool, home string) error {
	if noUI {
		if err := config.SetWebEnabled(l.Config, false); errors.Is(err, config.ErrWebEnabledSet) {
			return errors.New("config.toml sets [web] enabled, which --no-ui would change: change it there instead")
		} else if err != nil {
			return err
		}
	}
	return addWebAccess(l, access, home)
}

// addWebAccess writes setWebAccess's access, unless web.toml or config.toml sets one, or a Kode
// on this computer will hand its own over.
func addWebAccess(l paths.Layout, access, home string) error {
	if access == "" {
		if cat := extensions.Known["kode"]; cat.Find("kode", home, l.Root) != "" {
			return nil
		}
		access = config.AccessLocal
		if headless() {
			access = config.AccessRemote
		}
	}
	_, err := config.AddWeb(l.Config, config.Web{Access: access}, config.TLS{})
	return err
}

// headless reports whether this looks like a machine reached from elsewhere: an SSH session, or Linux
// with no display.
func headless() bool {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return true
	}
	return runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == ""
}

// linkMekoCLI makes ~/.local/bin/meko-cli for a release install whose meko command is there and
// whose meko-cli isn't: one installed before meko-cli existed, which an update reaches without meko
// install. Serve runs it once its release is past probation (makeMekoCLI). Anything already at that
// path stays.
func linkMekoCLI(app update.App, home string) error {
	t, err := os.Readlink(filepath.Join(home, ".local", "bin", progMeko))
	if err != nil || !strings.HasPrefix(t, app.Root+string(filepath.Separator)) {
		return nil //nolint:nilerr // no meko command of this install's: nothing to put meko-cli beside
	}
	if _, lerr := os.Lstat(filepath.Join(home, ".local", "bin", progMekoCLI)); lerr == nil {
		return nil
	}
	_, err = app.LinkCommandAs(home, progMekoCLI)
	return err
}

func onPath(dir string) bool {
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if d == dir {
			return true
		}
	}
	return false
}

// runUninstall removes the extensions, then the service, the meko command, the release installs,
// and the line meko install added to a shell's profile to put the command on the PATH. It asks
// nothing about the extensions: with Meko gone nothing would run them. Without purge each one is
// removed as meko extension delete does (its services, releases and command), and its data stays
// with Meko's; the first that fails stops it, before anything of Meko's is removed. With purge it
// first destroys the data folder as meko destroy does, the extensions' data with it (asking once,
// unless o.force), and then disconnects the agents that sent its token (disconnectAgents). It ends
// saying how to install Meko again.
func runUninstall(l paths.Layout, purge bool, o destroyOptions, stdout, stderr io.Writer) int {
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(stderr, err)
	}
	if purge {
		// Found while the data folder still says what this Meko's token is.
		disconnect := connectedAgents(l)
		o.uninstall = true
		if code := destroy(l, o, stdin, stdout, stderr); code != exitOK {
			return code
		}
		disconnect(stdout, stderr)
	} else if code := removeExtensions(l, stdout, stderr); code != exitOK {
		return code
	}
	if m, merr := newManager(); merr == nil && m.Installed() && (m.DataDir() == "" || paths.SameDir(m.DataDir(), l.Root)) {
		if err = m.Remove(); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "Removed the %s service\n", m.Kind())
	}
	if up, _ := running(l.Lock); up {
		if code := stopMeko(l, stdout, stderr); code != exitOK {
			return code
		}
	}
	app, _, ok := runningRelease()
	if !ok {
		if root, rerr := update.DefaultRoot(); rerr == nil {
			app = update.App{Root: root}
		}
	}
	if app.Root != "" {
		if err = app.UnlinkCommand(home); err != nil {
			return fail(stderr, err)
		}
		for _, link := range []string{progMekoCLI, progMekosh} {
			if err = app.UnlinkCommandAs(home, link); err != nil {
				return fail(stderr, err)
			}
		}
		if _, serr := os.Stat(app.Root); serr == nil {
			if err = app.Remove(); err != nil {
				return fail(stderr, err)
			}
		}
	}
	changed, err := removePathLines(home)
	for _, p := range changed {
		fmt.Fprintf(stdout, "Removed the PATH line meko install added to %s\n", homeRel(p, home))
	}
	if err != nil {
		fmt.Fprintf(stderr, "meko: can't remove the PATH line meko install added: %v\n", err)
	}
	if !purge {
		fmt.Fprintf(stdout, "Your data is still in %s; meko destroy (or meko uninstall --purge) removes it\n", l.Root)
	}
	fmt.Fprintln(stdout, "Meko removed. You can install it again using\n  `"+installCommand+"`")
	return exitOK
}

// removeExtensions removes every installed extension for uninstall, keeping its data: each one
// registered, or with a release Meko installed. It holds the update lock meanwhile.
func removeExtensions(l paths.Layout, stdout, stderr io.Writer) int {
	exts, err := findExtensionData(l)
	if err != nil {
		return fail(stderr, err)
	}
	lock := update.NewLock(l.Root)
	if len(exts) > 0 {
		if !lock.TryLock() {
			return fail(stderr, update.ErrLocked)
		}
		defer lock.Unlock()
	}
	for _, e := range exts {
		cat := extensions.Known[e.name]
		if _, serr := os.Stat(cat.App(l.Root, e.name).Root); !e.registered && (!cat.Managed || serr != nil) {
			continue // only its data is left, which stays
		}
		if err := removeExtension(l, e, false, stdout, stderr); err != nil {
			return fail(stderr, fmt.Errorf("couldn't remove %s: %w. Meko is still installed: run meko uninstall "+
				"again once that's fixed", extensionTitle(e.name), err))
		}
		if e.dir != "" {
			fmt.Fprintf(stdout, "Removed %s; its data stays in %s\n", extensionTitle(e.name), e.dir)
		} else {
			fmt.Fprintf(stdout, "Removed %s\n", extensionTitle(e.name))
		}
	}
	return exitOK
}

// connectedAgents finds the agents connected to this Meko (their meko server sends its token), and
// returns what disconnects them once the purge has removed the data folder and the token with it:
// their meko server would only fail. It only reads: unlike agentManager's readMCPAccess, it makes no
// data folder or token where there's none (then no agent can send it). The manager has no lock,
// since the lock is in the data folder the purge removes, and no meko uses it any more. An agent
// pointing elsewhere (another Meko on this computer, Meko Cloud) isn't connected, so it's left.
func connectedAgents(l paths.Layout) func(stdout, stderr io.Writer) {
	none := func(io.Writer, io.Writer) {}
	if !paths.LooksLikeDataDir(l.Root) {
		return none
	}
	cfg, err := config.Load(l.Config, os.Getenv, config.Overrides{})
	if err != nil {
		return none
	}
	ct, err := clientTokens(l, cfg)
	if err != nil {
		return none
	}
	tok, err := ct.Read()
	if err != nil {
		return none
	}
	port := cfg.Server.Port
	if up, _ := running(l.Lock); up {
		if p, perr := readInt(l.Port); perr == nil {
			port = p
		}
	}
	t := localTarget(mcpAccess{url: fmt.Sprintf("http://127.0.0.1:%d/mcp", port), token: tok})
	m := &agents.Manager{Target: func() (agents.Target, error) { return t, nil }}
	list, err := m.List()
	if err != nil {
		return none
	}
	var names []string
	for _, r := range list.Agents {
		if r.State == agents.StateConnected {
			names = append(names, r.Name)
		}
	}
	return func(stdout, stderr io.Writer) {
		var done []string
		for _, n := range names {
			r, derr := m.Disconnect(context.Background(), n)
			if derr != nil {
				fmt.Fprintf(stderr, "meko: %v. Remove its meko MCP server yourself\n", derr)
				continue
			}
			if r.Changed {
				done = append(done, r.Agent.Title)
			}
		}
		if len(done) > 0 {
			fmt.Fprintf(stdout, "Disconnected %s from Meko\n", joinAnd(done))
		}
	}
}

// runInstallExtension installs a known extension pointed at this Meko, with Meko's MCP URL and its
// client token, and registers it so meko start and stop run it. An extension that's already
// installed is set up again with them. Its install asks its own questions; o.extra (what followed
// -- on the command line) is passed on to it.
func runInstallExtension(l paths.Layout, name string, o extInstallOptions, stdout, stderr io.Writer) int {
	if _, ok := extensions.Known[name]; !ok {
		return fail(stderr, fmt.Errorf("no extension called %q: meko knows %s", name,
			strings.Join(extensions.KnownNames(), ", ")))
	}
	if up, _ := running(l.Lock); !up {
		return fail(stderr, errors.New("no Meko server is running: start it with meko start first"))
	}
	if err := installExtension(context.Background(), l, name, o, interactive(), os.Stdin, stdout, stderr); err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// extInstallOptions are extension install's: --version, a release to install (again) rather than
// the newest, and the arguments after --, for the extension's own install.
type extInstallOptions struct {
	version string
	extra   []string
}

// installExtension is runInstallExtension's work, for the running server's data folder at l, and
// for the web UI's Install too, which runs it with no terminal (interactive false, stdin nil).
func installExtension(ctx context.Context, l paths.Layout, name string, o extInstallOptions, interactive bool,
	stdin io.Reader, stdout, stderr io.Writer) error {
	cat, ok := extensions.Known[name]
	if !ok {
		return fmt.Errorf("no extension called %q: meko knows %s", name, strings.Join(extensions.KnownNames(), ", "))
	}
	cfg, err := config.Load(l.Config, os.Getenv, config.Overrides{})
	if err != nil {
		return err
	}
	tok, err := readClientToken(l, cfg)
	if err != nil {
		return err
	}
	port, err := readInt(l.Port)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	args := []string{"--meko-url", fmt.Sprintf("http://127.0.0.1:%d/mcp", port), "--meko-key", tok}
	if !interactive {
		args = append(args, "--non-interactive")
	}
	args = append(args, o.extra...)
	cmd := cat.Find(name, home, l.Root)
	if (cmd == "" || o.version != "") && cat.Managed {
		// Meko installs it from its release, then sets it up below as one already there.
		lock := update.NewLock(l.Root)
		if !lock.TryLock() {
			return update.ErrLocked
		}
		cmd, err = installRelease(ctx, l, cfg.Update, name, o.version, home, stdout)
		lock.Unlock()
		if err != nil {
			return err
		}
	}
	if cmd == "" {
		return fmt.Errorf("%s isn't installed, and Meko doesn't install it: put %s on PATH, then run this again",
			cat.Title, cat.Command)
	}
	if tooOld(cmd) {
		return fmt.Errorf("the installed %s predates --meko-url, which pointing it at this Meko "+
			"needs: update it (the Update button in Kode), then run this again", cat.Title)
	}
	fmt.Fprintf(stdout, "  %s %s is installed\n", spin.Mark(spin.OK, spin.Color(stdout)), cat.Title)
	env := extensions.CommandEnv(l.Root, cmd)
	if err = runAttached(ctx, cmd, append([]string{"install"}, args...), env, stdin, stdout, stderr); err != nil {
		return fmt.Errorf("%s failed: %w", cat.UserCommand("install"), err)
	}
	cmd = cat.Find(name, home, l.Root)
	if cmd == "" {
		return fmt.Errorf("%s was installed, but its program %s isn't there", cat.Title, cat.Command)
	}
	reg, err := extensions.Load(l.Root)
	if err != nil {
		return err
	}
	// Set up again (or another version installed), it keeps what it had: Update automatically
	// above all, which someone going back to a version turned off.
	e, ok := reg.Entries[name]
	if !ok {
		e.AddedAt = time.Now().UTC()
	}
	e.Command, e.InstalledByMeko = cmd, true
	reg.Entries[name] = e
	if err := reg.Save(); err != nil {
		return err
	}
	return nil
}

// readClientToken is the client token the running server at l takes, which extensions are given.
func readClientToken(l paths.Layout, cfg config.Config) (string, error) {
	ct, err := clientTokens(l, cfg)
	if err != nil {
		return "", err
	}
	return ct.Read()
}

// installRelease installs a Managed extension from its newest release (its bad versions skipped),
// or from the release of version, into its app folder, as Meko's own installs go: the tarball for
// this platform checked against SHA256SUMS and its signature, unpacked, its binary checked to run
// here, and app/current pointed at it (the ~/.local/bin links an older Meko made into this app
// folder removed). It returns app/current's program.
func installRelease(ctx context.Context, l paths.Layout, upd config.Update, name, version, home string,
	stdout io.Writer) (string, error) {
	cat := extensions.Known[name]
	dl, cancel := context.WithTimeout(ctx, extensions.InstallTimeout)
	defer cancel()
	var notices unsignedNotices
	c := releaseClient(upd, notices.add)
	looking := spin.Wait(stdout, "Looking for "+cat.Title+"'s releases")
	rels, err := c.Releases(dl)
	looking.Clear()
	if err != nil {
		return "", err // with no token, GitHub's refusal says why: its rate limit, or a private repo
	}
	app := cat.App(l.Root, name)
	goos, arch := update.Platform()
	var rel update.Release
	ver, ok := version, false
	if version == "" {
		rel, ver, ok = update.PickPackage(rels, cat.TagPrefixes, app.Packages(), goos, arch, app.BadVersions())
	} else {
		// That version, even one that was rolled back: it was asked for by name.
		rel, ok = update.PickPackageVersion(rels, cat.TagPrefixes, app.Packages(), version, goos, arch)
	}
	if !ok {
		what := "no release of " + cat.Title
		if version != "" {
			what = "no release " + version + " of " + cat.Title
		}
		return "", fmt.Errorf("%s for %s-%s was found in %s", what, goos, arch, c.Repo)
	}
	installing := spin.Start(stdout, "Release", fmt.Sprintf("installing %s %s", cat.Title, ver))
	// Owner-only, like the rest of the data folder; the extension's own folder may not be there yet.
	if err := os.MkdirAll(filepath.Dir(app.Root), 0o700); err != nil {
		installing.Fail()
		return "", err
	}
	if err := app.Stage(dl, c, rel, ver); err != nil {
		installing.Fail()
		return "", err
	}
	installing.Done(fmt.Sprintf("%s %s, downloaded and checked", cat.Title, ver))
	notices.print(stdout)
	if err := app.Repoint(ver); err != nil {
		return "", err
	}
	// A manual install supersedes a record of an update in progress.
	if err := app.ClearPending(); err != nil {
		return "", err
	}
	// People run it as meko <verb>: the ~/.local/bin links an older Meko made into this app folder go.
	return app.CurrentExe(), cat.UnlinkCommands(app, home)
}

// tooOld reports whether an installed Kode is a release from before --meko-url: its
// install.js (Kode's release layout, <version>/lib/dist/server/install.js beside bin/) doesn't name
// the flag. A layout it doesn't recognize (a build from source) isn't judged.
func tooOld(cmd string) bool {
	real, err := filepath.EvalSymlinks(cmd)
	if err != nil {
		return false
	}
	js := filepath.Join(filepath.Dir(filepath.Dir(real)), "lib", "dist", "server", "install.js")
	b, err := os.ReadFile(js) //nolint:gosec // G304: Kode's own file
	if err != nil {
		return false
	}
	return !strings.Contains(string(b), "meko-url")
}

// runAttached runs a command with env and stdin (the terminal, where an installer asks its
// questions, or nil for none), until it ends or ctx does. Without a terminal it runs in a process
// group of its own, so ending ctx stops what it started too (an installer's me_kode install).
func runAttached(ctx context.Context, name string, args, env []string, stdin io.Reader,
	stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: an extension's own command or installer
	cmd.Stdin, cmd.Stdout, cmd.Stderr, cmd.Env = stdin, stdout, stderr, env
	cmd.WaitDelay = extensions.PipeWait
	if stdin == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	}
	if err := cmd.Run(); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return errors.New("it took too long and was stopped")
		case ctx.Err() != nil:
			return errors.New("it was stopped: Meko is stopping")
		}
		return err
	}
	return nil
}

// interactive reports whether stdin is a terminal someone can answer questions on (/dev/null is a
// character device too, so the mode alone can't tell).
func interactive() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), ioctlGetTermios)
	return err == nil
}
