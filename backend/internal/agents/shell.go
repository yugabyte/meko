package agents

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// LoginPathWait is how long a list waits for the login shell's PATH the first time, within the 5 s
// AgentK gives the agents' list. LoginPathTimeout bounds the shell's run, which goes on in the
// background past the wait: a heavy profile can take seconds under a service, and the next list
// gets its PATH. LoginPathTTL is how long Find keeps its PATH before it reads it again in the
// background, so a list never waits for the shell but the first, and an agent installed since is
// still found.
var (
	LoginPathWait    = 3 * time.Second
	LoginPathTimeout = 20 * time.Second
	LoginPathTTL     = 5 * time.Minute
)

// Log, when set, hears of a login shell that gave no PATH, which would otherwise leave an agent
// missing from the list with no word why.
var Log *slog.Logger

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
// dirs is the last read that gave a PATH, which a failed read keeps. at is when the last read ended,
// either way. reading is closed when the read under way ends, nil with none.
var loginPathCache struct {
	sync.Mutex
	shell   string
	dirs    []string
	at      time.Time
	reading chan struct{}
}

// loginPath is the folders on the login shell's PATH. The first call starts reading them and waits
// up to LoginPathWait, and calls at the same time wait with it; once they're older than
// LoginPathTTL, a call gets them as they are and starts reading them again in the background.
func loginPath() []string {
	c := &loginPathCache
	c.Lock()
	shell := LoginShell
	if shell == "" {
		c.Unlock()
		return nil
	}
	if c.shell != shell {
		c.shell, c.dirs, c.at, c.reading = shell, nil, time.Time{}, nil
	}
	first := c.at.IsZero()
	if c.reading == nil && (first || time.Since(c.at) >= LoginPathTTL) {
		c.reading = make(chan struct{})
		go readInBackground(shell, LoginPathTimeout, Log, c.reading)
	}
	reading, wait := c.reading, LoginPathWait
	c.Unlock()
	if first {
		select {
		case <-reading:
		case <-time.After(wait):
		}
	}
	c.Lock()
	defer c.Unlock()
	return c.dirs
}

// readInBackground reads the login shell's PATH into loginPathCache, keeping the one it has when the
// shell gives none, and closes done.
func readInBackground(shell string, timeout time.Duration, log *slog.Logger, done chan struct{}) {
	defer close(done)
	dirs, err := readLoginPath(shell, timeout)
	c := &loginPathCache
	c.Lock()
	if c.reading == done {
		c.reading = nil
	}
	if c.shell == shell {
		c.at = time.Now()
		if err == nil {
			c.dirs = dirs
		}
	}
	c.Unlock()
	if err != nil && log != nil {
		log.Warn("the login shell gave no PATH: agents only on it aren't found", "shell", shell, "error", err.Error())
	}
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
// then fail, or hang after (a profile's command): only a missing or unclosed pair gives none, and
// the error says why. It
// runs in a session of its own, away from any terminal, so an interactive shell can't take it over,
// and its whole group is killed at the timeout.
func readLoginPath(shell string, timeout time.Duration) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	name, args := shellArgs(shell, "printf '"+pathMark+"%s"+pathMark+"' \"$PATH\"")
	c := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: the user's own shell
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = time.Second
	out, err := c.Output()
	_, rest, ok := strings.Cut(string(out), pathMark)
	p, _, closed := strings.Cut(rest, pathMark)
	switch {
	case ok && closed:
	case ctx.Err() != nil:
		return nil, fmt.Errorf("it took more than %s", timeout)
	case err != nil:
		return nil, err
	default:
		return nil, errors.New("it printed none")
	}
	var dirs []string
	for _, d := range filepath.SplitList(p) {
		if filepath.IsAbs(d) {
			dirs = append(dirs, d)
		}
	}
	return dirs, nil
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
