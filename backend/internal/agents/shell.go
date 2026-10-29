package agents

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// LoginShell is the user's shell, whose PATH Find looks on last, when Meko runs as a service
// (MEKO_SERVICE): a service starts with a thin PATH, and version managers (nvm, asdf, mise) put an
// agent's folder on PATH only in the user's shell. It's $SHELL, which systemd and launchd set for a
// user's service, else /bin/sh. Outside a service, Meko was started from the user's shell and has its
// PATH already, so it's "": none, as tests have it.
var LoginShell = defaultShell()

// LoginPathTimeout bounds a run of the login shell, within the 5 s AgentK gives the agents' list.
// LoginPathTTL is how long Find keeps its PATH (or that it gave none) before it reads it again in the
// background, so a list never waits for the shell but the first, and an agent installed since is
// still found.
var (
	LoginPathTimeout = 3 * time.Second
	LoginPathTTL     = 5 * time.Minute
)

func defaultShell() string {
	if os.Getenv("MEKO_SERVICE") == "" {
		return ""
	}
	if s := os.Getenv("SHELL"); filepath.IsAbs(s) {
		return s
	}
	return "/bin/sh"
}

// loginPathCache is the login shell's PATH last read: Find runs from the server's routes at once.
var loginPathCache struct {
	sync.Mutex
	shell      string
	dirs       []string
	at         time.Time
	refreshing bool
}

// loginPath is the folders on the login shell's PATH. The first call reads them, and calls at the
// same time wait for it; once they're older than LoginPathTTL, a call gets them as they are and
// starts reading them again in the background.
func loginPath() []string {
	c := &loginPathCache
	c.Lock()
	defer c.Unlock()
	shell, timeout := LoginShell, LoginPathTimeout
	switch {
	case shell == "":
		return nil
	case c.shell != shell || c.at.IsZero():
		c.shell, c.dirs, c.at = shell, readLoginPath(shell, timeout), time.Now()
	case time.Since(c.at) >= LoginPathTTL && !c.refreshing:
		c.refreshing = true
		go func() {
			dirs := readLoginPath(shell, timeout)
			c.Lock()
			defer c.Unlock()
			c.refreshing = false
			if c.shell == shell {
				c.dirs, c.at = dirs, time.Now()
			}
		}()
	}
	return c.dirs
}

// loginPathOf is the login shell's PATH as last read when cmd is in one of its folders, else nil:
// a command found there runs with it, so what it runs in turn (an npm install's node, a version
// manager's shim's tool) is found as in the user's shell. It never runs the shell.
func loginPathOf(cmd string) []string {
	c := &loginPathCache
	c.Lock()
	defer c.Unlock()
	if LoginShell == "" || c.shell != LoginShell {
		return nil
	}
	dir := filepath.Dir(cmd)
	for _, d := range c.dirs {
		if filepath.Clean(d) == dir {
			return c.dirs
		}
	}
	return nil
}

// pathMark brackets the PATH the shell prints, since a profile may print more.
const pathMark = "__meko_path__"

// readLoginPath runs the shell to print its PATH between two pathMarks, which it may print and
// then fail, or hang after (a profile's command): only a missing or unclosed pair gives none. It
// runs in a session of its own, away from any terminal, so an interactive shell can't take it over,
// and its whole group is killed at the timeout.
func readLoginPath(shell string, timeout time.Duration) []string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	name, args := shellArgs(shell, "printf '"+pathMark+"%s"+pathMark+"' \"$PATH\"")
	c := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: the user's own shell
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = time.Second
	out, _ := c.Output()
	_, rest, ok := strings.Cut(string(out), pathMark)
	p, _, closed := strings.Cut(rest, pathMark)
	if !ok || !closed {
		return nil
	}
	var dirs []string
	for _, d := range filepath.SplitList(p) {
		if filepath.IsAbs(d) {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// shellArgs runs script in the login shell. bash, zsh and fish run it interactive too, as a
// terminal's shell, since nvm and the like set PATH up in ~/.zshrc and ~/.bashrc. A login bash reads
// ~/.bashrc only when its profile sources it, as Ubuntu's ~/.profile does and as a macOS terminal,
// whose shells are login shells, also needs. Any other shell (csh's -c takes no -l) gives way to
// sh's login profile. fish joins "$PATH" with colons, as a path variable.
func shellArgs(shell, script string) (string, []string) {
	switch filepath.Base(shell) {
	case "bash", "zsh", "fish":
		return shell, []string{"-i", "-l", "-c", script}
	}
	return "/bin/sh", []string{"-l", "-c", script}
}
