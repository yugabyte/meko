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
	"time"

	"golang.org/x/sys/unix"

	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/extensions"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/service"
	"github.com/yugabyte/meko/backend/internal/update"
)

// newReleaseClient reads a repo's GitHub releases; tests replace it.
var newReleaseClient = update.NewClient

// runningRelease finds the release install the running binary belongs to; tests replace it.
var runningRelease = update.Running

// runInstall sets this meko up: as a release install, app/current and the meko command point at
// it; then the client token, the web listener's access, and the login service, which it starts. It
// installs Meko alone: extensions are meko install_extension's.
func runInstall(l paths.Layout, noService bool, access string, stdout, stderr io.Writer) int {
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
		exe = app.CurrentExe()
		fmt.Fprintf(stdout, "Installed meko %s in %s\n", ver, app.Dir(ver))
		if onPath(filepath.Dir(link)) {
			fmt.Fprintf(stdout, "  Command: meko (%s)\n", link)
		} else {
			fmt.Fprintf(stdout, "  Command: %s (add %s to your PATH to run meko directly)\n", link, filepath.Dir(link))
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
	fmt.Fprintf(stdout, "  Data:    %s\n  Secrets: %s\n", l.Root, map[string]string{
		"keychain": "in your login Keychain", "file": "in the data folder, readable only by you"}[ct.Store.Kind()])
	if err = setWebAccess(l, cfg, access, home, stdout); err != nil {
		return fail(stderr, err)
	}
	if noService {
		fmt.Fprintln(stdout, "No service was set up (--no-service): start Meko with meko start")
		return nextSteps(stdout)
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
	fmt.Fprintf(stdout, "  Service: %s (%s), starts at login and restarts on failure\n", m.Kind(), m.File())
	if sd, ok := m.(*service.Systemd); ok {
		if ok, detail, note := sd.Linger(); ok {
			fmt.Fprintf(stdout, "  Boot:    %s\n", detail)
		} else {
			fmt.Fprintf(stdout, "  Boot:    %s. %s\n", detail, note)
		}
	}
	st, ok := waitAnswering(l, serviceStartTimeout, pid)
	if !ok {
		return fail(stderr, fmt.Errorf("the service didn't answer within %s. Check: %s, and meko logs",
			serviceStartTimeout, m.StatusHint()))
	}
	fmt.Fprintf(stdout, "Meko is running at http://127.0.0.1:%d (pid %d)\n", st.Port, st.PID)
	if st.Web != nil {
		fmt.Fprintf(stdout, "  Web UI:  %s\n", st.Web.URL)
	}
	return nextSteps(stdout)
}

// setWebAccess writes the web listener's access to web.toml (config.AddWeb) when neither it nor
// config.toml has a [web] table: with access, or, unless an
// AgentK is on this computer (whose own access Meko takes over when it hands over its port), with
// the default AgentK had: remote over SSH or on Linux with no display, local otherwise.
func setWebAccess(l paths.Layout, cfg config.Config, access, home string, stdout io.Writer) error {
	if access == "" {
		if cat := extensions.Known["agentk"]; cat.Find("agentk", home, l.Root) != "" {
			return nil
		}
		access = config.AccessLocal
		if headless() {
			access = config.AccessRemote
		}
	}
	added, err := config.AddWeb(l.Config, config.Web{Access: access}, config.TLS{})
	if err != nil || !added {
		return err
	}
	cfg.Web.Access = access
	note := "browsers on this computer open it with no login"
	if access == config.AccessRemote {
		note = "every address, over HTTPS; meko setup_link makes the login"
	}
	fmt.Fprintf(stdout, "  Web UI:  %s access (%s), in %s; [web] in config.toml overrides it\n", access, note,
		config.WebFile(l.Config))
	return nil
}

// headless reports whether this looks like a machine reached from elsewhere: an SSH session, or Linux
// with no display.
func headless() bool {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return true
	}
	return runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == ""
}

func nextSteps(stdout io.Writer) int {
	fmt.Fprint(stdout, `
Next:
  meko status                      how it's doing
  meko logs                        what it's logging
  meko token --mcp                 the command that connects Claude Code to Meko
  meko install_extension agentk    add AgentK, which uses this Meko
`)
	return exitOK
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
	if m, merr := newManager(); merr == nil && m.Installed() && (m.DataDir() == l.Root || m.DataDir() == "") {
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
		if _, serr := os.Stat(app.Root); serr == nil {
			if err = app.Remove(); err != nil {
				return fail(stderr, err)
			}
			fmt.Fprintf(stdout, "Removed the releases in %s and the meko command\n", app.Root)
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
// installed is set up again with them. Nothing else is passed to its installer: it asks its own
// questions.
func runInstallExtension(l paths.Layout, name string, stdout, stderr io.Writer) int {
	if _, ok := extensions.Known[name]; !ok {
		return fail(stderr, fmt.Errorf("no extension called %q: meko knows agentk", name))
	}
	if up, _ := running(l.Lock); !up {
		return fail(stderr, errors.New("no Meko server is running: start it with meko start first"))
	}
	if err := installExtension(context.Background(), l, name, interactive(), os.Stdin, stdout, stderr); err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// installExtension is runInstallExtension's work, for the running server's data folder at l, and
// for the web UI's Install too, which runs it with no terminal (interactive false, stdin nil).
func installExtension(ctx context.Context, l paths.Layout, name string, interactive bool, stdin io.Reader,
	stdout, stderr io.Writer) error {
	cat, ok := extensions.Known[name]
	if !ok {
		return fmt.Errorf("no extension called %q: meko knows agentk", name)
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
	if cmd := cat.Find(name, home, l.Root); cmd != "" {
		if tooOld(cmd) {
			return fmt.Errorf("the installed %s predates --meko-url, which pointing it at this Meko "+
				"needs: update it (the Update button in AgentK), then run this again", cat.Title)
		}
		fmt.Fprintf(stdout, "%s is installed (%s): setting it up to use this Meko\n", cat.Title, cmd)
		err = runAttached(ctx, cmd, append([]string{"install"}, args...), extensions.Env(l.Root), stdin, stdout, stderr)
		if err != nil {
			return fmt.Errorf("%s install failed: %w", cat.Command, err)
		}
	} else if err = runExtensionInstaller(ctx, l, cfg.Update.Repo, cat, args, stdin, stdout, stderr); err != nil {
		return err
	}
	cmd := cat.Find(name, home, l.Root)
	if cmd == "" {
		return fmt.Errorf("%s was installed, but %s isn't on PATH or in ~/.local/bin", cat.Title, cat.Command)
	}
	reg, err := extensions.Load(l.Root)
	if err != nil {
		return err
	}
	reg.Entries[name] = extensions.Entry{Command: cmd, InstalledByMeko: true, AddedAt: time.Now().UTC()}
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

// runExtensionInstaller downloads the newest release's installer of an extension and runs it, with
// MEKO_HOME naming the data folder.
func runExtensionInstaller(ctx context.Context, l paths.Layout, repo string, cat extensions.Catalog, args []string,
	stdin io.Reader, stdout, stderr io.Writer) error {
	dl, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	c := newReleaseClient(repo)
	rels, err := c.Releases(dl)
	if err != nil {
		return err
	}
	rel, ver, ok := update.Pick(rels, cat.TagPrefixes, func(string) []string { return []string{cat.Installer} }, nil)
	if !ok {
		return fmt.Errorf("no release of %s with %s was found in %s", cat.Title, cat.Installer, c.Repo)
	}
	a, _ := rel.Asset(cat.Installer)
	dir, err := os.MkdirTemp("", "meko-ext-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	script := filepath.Join(dir, cat.Installer)
	if err = c.Download(dl, a, script); err != nil {
		return err
	}
	// Releases from before Meko's extensions refuse --meko-url, and their installer looks for the
	// repo's latest release, which is Meko's now.
	b, rerr := os.ReadFile(script) //nolint:gosec // G304: our download
	if rerr != nil || !strings.Contains(string(b), "--meko-url") {
		return fmt.Errorf("the newest %s release (%s) is older than this Meko and can't use it yet: it comes "+
			"with the next %s release", cat.Title, rel.Tag, cat.Title)
	}
	fmt.Fprintf(stdout, "Installing %s %s (%s)\n", cat.Title, ver, rel.Tag)
	err = runAttached(ctx, "sh", append([]string{script}, args...), extensions.Env(l.Root), stdin, stdout, stderr)
	if err != nil {
		return fmt.Errorf("%s's installer failed: %w", cat.Title, err)
	}
	return nil
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
// questions, or nil for none), until it ends or ctx does.
func runAttached(ctx context.Context, name string, args, env []string, stdin io.Reader,
	stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: an extension's own command or installer
	cmd.Stdin, cmd.Stdout, cmd.Stderr, cmd.Env = stdin, stdout, stderr, env
	cmd.WaitDelay = extensions.PipeWait
	if err := cmd.Run(); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		if ctx.Err() != nil {
			return fmt.Errorf("stopped: %w", ctx.Err())
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

// runUninstallExtension stops starting and stopping an extension with Meko. It stays installed.
func runUninstallExtension(l paths.Layout, name string, stdout, stderr io.Writer) int {
	reg, err := extensions.Load(l.Root)
	if err != nil {
		return fail(stderr, err)
	}
	e, ok := reg.Entries[name]
	if !ok {
		return fail(stderr, fmt.Errorf("extension %q isn't registered (registered: %s)", name,
			strings.Join(append(reg.Names(), "none")[:max(len(reg.Names()), 1)], ", ")))
	}
	delete(reg.Entries, name)
	if err := reg.Save(); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "meko start and stop no longer run %s. It's still installed: %s uninstall removes it\n",
		name, e.Command)
	return exitOK
}
