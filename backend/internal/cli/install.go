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

	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/extensions"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/service"
	"github.com/yugabyte/meko/backend/internal/spin"
	"github.com/yugabyte/meko/backend/internal/update"
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

// runningRelease finds the release install the running binary belongs to; tests replace it.
var runningRelease = update.Running

// runInstall sets this meko up: as a release install, app/current and the meko command point at
// it; then the client token, the web listener's access, and the login service, which it starts. It
// installs Meko alone: extensions are meko extension install's.
func runInstall(l paths.Layout, noService bool, access string, noUI bool, stdout, stderr io.Writer) int {
	r := newReport(stdout)
	if access != "" && access != config.AccessLocal && access != config.AccessRemote {
		return fail(stderr, fmt.Errorf("--access takes local or remote, not %q", access))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(stderr, err)
	}
	exe, err := os.Executable()
	if err != nil {
		return fail(stderr, err)
	}
	if app, ver, ok := runningRelease(); ok {
		if err = app.Repoint(ver); err != nil {
			return fail(stderr, err)
		}
		// Installing by hand supersedes an update on probation.
		if err = app.ClearPending(); err != nil {
			return fail(stderr, err)
		}
		var link string
		if link, err = app.LinkCommand(home); err != nil {
			return fail(stderr, err)
		}
		if _, err = app.LinkCommandAs(home, progMekoCLI); err != nil {
			return fail(stderr, err)
		}
		exe = app.CurrentExe()
		fmt.Fprintf(stdout, "Installing Meko %s in %s…\n", ver, app.Dir(ver))
		if bin := filepath.Dir(link); onPath(bin) {
			r.step(stepOK, "Commands", "meko and meko-cli, in "+bin)
		} else {
			r.step(stepWarn, "Commands", fmt.Sprintf("meko and meko-cli, in %s (add it to your PATH to run them directly)", bin))
		}
	} else {
		fmt.Fprintf(stdout, "Setting up this build of meko (%s). It isn't from a release, "+
			"so it doesn't update itself\n", exe)
	}
	if err = l.Ensure(); err != nil {
		return fail(stderr, err)
	}
	cfg, err := config.Load(l.Config, os.Getenv, config.Overrides{})
	if err != nil {
		return fail(stderr, err)
	}
	ct, err := clientTokens(l, cfg)
	if err != nil {
		return fail(stderr, err)
	}
	if _, err = ct.Ensure(); err != nil {
		return fail(stderr, err)
	}
	r.step(stepOK, "Data", fmt.Sprintf("%s (secrets %s)", l.Root, map[string]string{
		"keychain": "in your login Keychain", "file": "in it, readable only by you"}[ct.Store.Kind()]))
	if err = setWebAccess(l, access, noUI, home, stdout); err != nil {
		return fail(stderr, err)
	}
	if noService {
		r.step(stepWarn, "Service", "none (--no-service): start Meko with meko start")
		r.next([][2]string{
			{"meko start", "start Meko"},
			{"meko status", "check on Meko at any time"},
		})
		return exitOK
	}
	m, err := newManager()
	if err != nil {
		return fail(stderr, err)
	}
	if err := m.Preflight(); err != nil {
		return fail(stderr, err)
	}
	if other := m.DataDir(); m.Installed() && other != "" && other != l.Root {
		fmt.Fprintf(stdout, "The service ran the data folder %s; it now runs %s\n", other, l.Root)
	}
	// A server run by hand holds the data folder, and the service's own couldn't start.
	if up, _ := running(l.Lock); up && serviceOf(l) == nil {
		if code := stopMeko(l, stdout, stderr); code != exitOK {
			return code
		}
	}
	host, _ := os.Hostname()
	pid := currentPID(l)
	if err := m.Install(service.Spec{Exe: exe, DataDir: l.Root, ServiceLog: l.ServiceLog, Host: host}); err != nil {
		return fail(stderr, err)
	}
	r.step(stepOK, "Service", fmt.Sprintf("%s (%s), starts at login and restarts on failure", m.Kind(), m.File()))
	if sd, ok := m.(*service.Systemd); ok {
		if ok, detail, note := sd.Linger(); ok {
			r.step(stepOK, "Boot", detail)
		} else {
			r.step(stepWarn, "Boot", detail+". "+note)
		}
	}
	st, ok := waitAnswering(l, serviceStartTimeout, pid)
	if !ok {
		return fail(stderr, fmt.Errorf("the service didn't answer within %s. Check: %s, and meko logs",
			serviceStartTimeout, m.StatusHint()))
	}
	r.step(stepOK, "Server", fmt.Sprintf("running at http://127.0.0.1:%d (pid %d)", st.Port, st.PID))
	next := [][2]string{{"meko ui", "open the web UI"}, {"meko status", "check on Meko at any time"}}
	if noUI {
		next[0] = [2]string{"meko ui start", "turn the web UI on, when you want it"}
	}
	if !agentKRegistered(l) {
		next = append(next, [2]string{"meko extension install agentk", "add AgentK, your coding team"})
	}
	next = append(next, [2]string{"meko agent connect claude",
		"connect Claude Code to Meko (or codex, kiro, opencode, cursor)"})
	printRunning(l, "is installed and running", next, stdout)
	return exitOK
}

// setWebAccess writes the web listener's access to web.toml (config.AddWeb) when neither it nor
// config.toml sets one: with access, or, unless an AgentK is on this computer (whose own access Meko
// takes over when it hands over its port), with the default AgentK had: remote over SSH or on Linux
// with no display, local otherwise. With noUI it turns the web UI off there too (config.SetWebEnabled).
func setWebAccess(l paths.Layout, access string, noUI bool, home string, stdout io.Writer) error {
	if noUI {
		if err := config.SetWebEnabled(l.Config, false); errors.Is(err, config.ErrWebEnabledSet) {
			return errors.New("config.toml sets [web] enabled, which --no-ui would change: change it there instead")
		} else if err != nil {
			return err
		}
	}
	added, err := addWebAccess(l, access, home)
	switch {
	case err != nil:
		return err
	case noUI:
		newReport(stdout).step(stepOK, "Web UI", fmt.Sprintf("%s, in %s", webOffNote, config.WebFile(l.Config)))
	case added != "":
		note := "from this computer"
		if added == config.AccessRemote {
			note = "every address, over HTTPS, no login"
		}
		newReport(stdout).step(stepOK, "Web UI", fmt.Sprintf("%s access (%s), in %s; [web] in config.toml overrides it",
			added, note, config.WebFile(l.Config)))
	}
	return nil
}

// addWebAccess is setWebAccess's access: the one it wrote, or "" when it wrote none.
func addWebAccess(l paths.Layout, access, home string) (string, error) {
	if access == "" {
		if cat := extensions.Known["agentk"]; cat.Find("agentk", home, l.Root) != "" {
			return "", nil
		}
		access = config.AccessLocal
		if headless() {
			access = config.AccessRemote
		}
	}
	added, err := config.AddWeb(l.Config, config.Web{Access: access}, config.TLS{})
	if err != nil || !added {
		return "", err
	}
	return access, nil
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

// runUninstall removes the service, the meko command and the release installs. The data stays
// unless purge, which first destroys it as meko destroy does (asking, unless o.force; the
// extensions go too with o.withExtensions or when told so), with the client token wherever it's
// kept. Extensions stay installed unless that destroy removed them.
func runUninstall(l paths.Layout, purge bool, o destroyOptions, stdout, stderr io.Writer) int {
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(stderr, err)
	}
	// Read before a purge removes the data folder, which holds the extension list.
	var exts []string
	if reg, rerr := extensions.Load(l.Root); rerr == nil {
		for _, n := range reg.Names() {
			exts = append(exts, fmt.Sprintf("Extension %s is still installed: %s uninstall removes it", n,
				reg.Entries[n].Command))
		}
	}
	if purge {
		code, kept := destroy(l, o, stdin, stdout, stderr)
		if code != exitOK {
			return code
		}
		if !kept {
			exts = nil
		}
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
			fmt.Fprintf(stdout, "Removed the releases in %s and the meko and meko-cli commands\n", app.Root)
		}
	}
	for _, line := range exts {
		fmt.Fprintln(stdout, line)
	}
	if !purge {
		fmt.Fprintf(stdout, "Your data is still in %s; meko destroy (or meko uninstall --purge) removes it\n", l.Root)
	}
	return exitOK
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
			"needs: update it (the Update button in AgentK), then run this again", cat.Title)
	}
	fmt.Fprintf(stdout, "%s is installed (%s): setting it up to use this Meko\n", cat.Title, cmd)
	env := extensions.CommandEnv(l.Root, cmd)
	if err = runAttached(ctx, cmd, append([]string{"install"}, args...), env, stdin, stdout, stderr); err != nil {
		return fmt.Errorf("%s install failed: %w", cat.Command, err)
	}
	cmd = cat.Find(name, home, l.Root)
	if cmd == "" {
		return fmt.Errorf("%s was installed, but %s isn't on PATH or in ~/.local/bin", cat.Title, cat.Command)
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
	fmt.Fprintf(stdout, "%s uses this Meko, and meko start and meko stop start and stop it too\n", cat.Title)
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
// here, app/current pointed at it, and ~/.local/bin/<command> at app/current unless that's another
// data folder's (its old names' links into this app folder removed). It returns the link, or
// app/current's command when there's none.
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
	rel, ver, ok := update.Release{}, version, false
	if version == "" {
		rel, ver, ok = update.PickPackage(rels, cat.TagPrefixes, name, goos, arch, app.BadVersions())
	} else {
		// That version, even one that was rolled back: it was asked for by name.
		rel, ok = update.PickPackageVersion(rels, cat.TagPrefixes, name, version, goos, arch)
	}
	if !ok {
		what := "no release of " + cat.Title
		if version != "" {
			what = "no release " + version + " of " + cat.Title
		}
		return "", fmt.Errorf("%s for %s-%s was found in %s", what, goos, arch, c.Repo)
	}
	installing := spin.Start(stdout, "Release", fmt.Sprintf("installing %s %s (%s)", cat.Title, ver, rel.Tag))
	// Owner-only, like the rest of the data folder; the extension's own folder may not be there yet.
	if err := os.MkdirAll(filepath.Dir(app.Root), 0o700); err != nil {
		installing.Fail()
		return "", err
	}
	if err := app.Stage(dl, c, rel, ver); err != nil {
		installing.Fail()
		return "", err
	}
	installing.Done(fmt.Sprintf("%s %s (%s), downloaded and checked", cat.Title, ver, rel.Tag))
	notices.print(stdout)
	if err := app.Repoint(ver); err != nil {
		return "", err
	}
	// A manual install supersedes a record of an update in progress.
	if err := app.ClearPending(); err != nil {
		return "", err
	}
	if !app.MayLink(home) {
		fmt.Fprintf(stdout, "~/.local/bin/%s runs another Meko data folder's %s, so it stays; this one is %s\n",
			cat.Command, cat.Title, app.CurrentExe())
		return app.CurrentExe(), nil
	}
	link, lerr := app.LinkCommand(home)
	if lerr != nil {
		return "", lerr
	}
	// Its command's old names (agentk before me_koder) go once the new one is there.
	return link, cat.UnlinkOldCommands(app, home)
}

// tooOld reports whether an installed AgentK is a release from before --meko-url: its
// install.js (AgentK's release layout, <version>/lib/dist/server/install.js beside bin/) doesn't name
// the flag. A layout it doesn't recognize (a build from source) isn't judged.
func tooOld(cmd string) bool {
	real, err := filepath.EvalSymlinks(cmd)
	if err != nil {
		return false
	}
	js := filepath.Join(filepath.Dir(filepath.Dir(real)), "lib", "dist", "server", "install.js")
	b, err := os.ReadFile(js) //nolint:gosec // G304: AgentK's own file
	if err != nil {
		return false
	}
	return !strings.Contains(string(b), "meko-url")
}

// runAttached runs a command with env and stdin (the terminal, where an installer asks its
// questions, or nil for none), until it ends or ctx does. Without a terminal it runs in a process
// group of its own, so ending ctx stops what it started too (an installer's me_koder install).
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
