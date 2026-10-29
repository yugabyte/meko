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

// LoginShell is the user's shell ($SHELL, which systemd and launchd set for a user's service, else
// /bin/sh), whose PATH Find looks on last: a service starts with a thin PATH, and version managers
// (nvm, asdf, mise) put an agent's folder on PATH only in the user's shell. "" looks on none: tests
// empty it.
var LoginShell = defaultShell()

// LoginPathTimeout bounds a run of the login shell, and LoginPathTTL is how long Find keeps its PATH
// (or that it gave none), so a list doesn't start a shell each time and an agent installed since is
// still found.
var (
	LoginPathTimeout = 10 * time.Second
	LoginPathTTL     = 5 * time.Minute
)

func defaultShell() string {
	if s := os.Getenv("SHELL"); filepath.IsAbs(s) {
		return s
	}
	return "/bin/sh"
}

// loginPathCache is the login shell's PATH last read: Find runs from the server's routes at once.
var loginPathCache struct {
	sync.Mutex
	shell string
	dirs  []string
	at    time.Time
}

// loginPath is the folders on the login shell's PATH, read at most once a LoginPathTTL.
func loginPath() []string {
	c := &loginPathCache
	c.Lock()
	defer c.Unlock()
	if LoginShell == "" {
		return nil
	}
	if c.shell != LoginShell || c.at.IsZero() || time.Since(c.at) >= LoginPathTTL {
		c.shell, c.dirs, c.at = LoginShell, readLoginPath(LoginShell), time.Now()
	}
	return c.dirs
}

// pathMark brackets the PATH the shell prints, since a profile may print more.
const pathMark = "__meko_path__"

// readLoginPath runs the shell to print its PATH: none when it fails or takes longer than
// LoginPathTimeout. It runs in a session of its own, away from any terminal, so an interactive
// shell can't take it over, and its whole group is killed at the timeout.
func readLoginPath(shell string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), LoginPathTimeout)
	defer cancel()
	name, args := shellArgs(shell, "printf '"+pathMark+"%s"+pathMark+"' \"$PATH\"")
	c := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: the user's own shell
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = time.Second
	out, _ := c.Output() // a profile's failing command may fail the shell after it printed the PATH
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
// terminal's shell, since nvm and the like set PATH up in ~/.bashrc and ~/.zshrc; any other shell
// (csh's -c takes no -l) gives way to sh's login profile.
func shellArgs(shell, script string) (string, []string) {
	switch filepath.Base(shell) {
	case "bash", "zsh", "fish":
		return shell, []string{"-i", "-l", "-c", script}
	}
	return "/bin/sh", []string{"-l", "-c", script}
}
