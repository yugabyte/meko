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

// LoginPathWait is how long after the first read of the login shell's PATH starts the lists wait
// for it, all of them together, within the 5 s Kode gives the agents' list. LoginPathTimeout
// bounds the shell's run, which goes on in the background past the wait: a heavy profile can take
// seconds under a service, and the lists after it ends get its PATH. LoginPathTTL is how long Find
// keeps its PATH before it reads it again in the background, so no list waits for the shell after
// the first read, and an agent installed since is still found.
var (
	LoginPathWait    = 3 * time.Second
	LoginPathTimeout = 20 * time.Second
	LoginPathTTL     = 5 * time.Minute
)

// Log, when set, hears of a login shell that gave no PATH: Find keeps the one it gave last, and with
// none the agents only on it are missing from the list, which would otherwise say nothing of why.
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
// either way. reading is closed when the read under way ends, nil with none, and stop kills its
// shell. waitUntil is when the first read stops being waited for.
var loginPathCache struct {
	sync.Mutex
	shell     string
	dirs      []string
	at        time.Time
	reading   chan struct{}
	stop      context.CancelFunc
	waitUntil time.Time
}

// loginPath is the folders on the login shell's PATH. The first call starts reading them, and it and
// every call until the read ends wait for it until LoginPathWait after it started, no longer, so one
// list's Finds wait LoginPathWait in all; once they're older than LoginPathTTL, a call gets them as
// they are and starts reading them again in the background.
func loginPath() []string {
	c := &loginPathCache
	c.Lock()
	shell := LoginShell
	if shell == "" {
		c.Unlock()
		return nil
	}
	if c.shell != shell {
		c.shell, c.dirs, c.at, c.reading, c.stop = shell, nil, time.Time{}, nil, nil
	}
	first := c.at.IsZero()
	if c.reading == nil && (first || time.Since(c.at) >= LoginPathTTL) {
		var ctx context.Context
		ctx, c.stop = context.WithCancel(context.Background())
		c.reading = make(chan struct{})
		if first {
			c.waitUntil = time.Now().Add(LoginPathWait)
		}
		go readInBackground(ctx, c.stop, shell, LoginPathTimeout, Log, c.reading)
	}
	reading, wait := c.reading, time.Until(c.waitUntil)
	c.Unlock()
	if first && wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-reading:
		case <-t.C:
		}
	}
	c.Lock()
	defer c.Unlock()
	return c.dirs
}

// readInBackground reads the login shell's PATH into loginPathCache, keeping the one it has when the
// shell gives none, and closes done.
func readInBackground(ctx context.Context, stop context.CancelFunc, shell string, timeout time.Duration,
	log *slog.Logger, done chan struct{}) {
	defer close(done)
	defer stop()
	dirs, err := readLoginPath(ctx, shell, timeout)
	stopped := errors.Is(err, context.Canceled)
	c := &loginPathCache
	c.Lock()
	if c.reading == done {
		c.reading, c.stop = nil, nil
	}
	kept := false
	if c.shell == shell && !stopped {
		c.at = time.Now()
		if err == nil {
			c.dirs = dirs
		}
		kept = c.dirs != nil
	}
	c.Unlock()
	switch {
	case err == nil || stopped || log == nil:
	case kept:
		log.Warn("the login shell gave no PATH: keeping the one it gave last", "shell", shell, "error", err.Error())
	default:
		log.Warn("the login shell gave no PATH: agents only on it aren't found", "shell", shell, "error", err.Error())
	}
}

// StopLoginShell kills the login shell a read has running, its children too, and waits for the read
// to end, for serve's stop: the shell runs in a session of its own, which a service manager that
// kills only the job's process group (launchd) leaves running, and a hanging profile never ends.
func StopLoginShell() {
	c := &loginPathCache
	c.Lock()
	reading, stop := c.reading, c.stop
	c.Unlock()
	if reading == nil {
		return
	}
	stop()
	<-reading
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
// the error says why. It runs in a session of its own, away from any terminal, so an interactive
// shell can't take it over, and its whole group is killed at the timeout, or when ctx ends.
func readLoginPath(ctx context.Context, shell string, timeout time.Duration) ([]string, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
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
	case parent.Err() != nil:
		return nil, parent.Err()
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
