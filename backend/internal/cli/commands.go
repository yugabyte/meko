package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yugabyte/meko/backend/internal/auth"
	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/store"
	"github.com/yugabyte/meko/backend/internal/version"
)

// Timeouts for start and stop. stop waits longer than serve's own shutdown (drain, then
// children), so its SIGKILL is only for a server that hangs.
var (
	startTimeout = 10 * time.Second
	stopTimeout  = drainTimeout + childStopTimeout + 5*time.Second
)

// runStart starts serve in the background, in its own session, and waits until it answers.
// The server's own stderr goes to logs/start.log, which is printed if it never answers, so
// errors from before logging starts aren't lost.
func runStart(l paths.Layout, o config.Overrides, stdout, stderr io.Writer) int {
	if up, err := running(l.Lock); err != nil {
		return fail(stderr, err)
	} else if up {
		if port, err := readInt(l.Port); err == nil {
			fmt.Fprintf(stdout, "Meko is already running at http://127.0.0.1:%d (data in %s).\n", port, l.Root)
		} else {
			fmt.Fprintf(stdout, "Meko is already running, and still starting (data in %s).\n", l.Root)
		}
		return exitOK
	}
	if err := l.Ensure(); err != nil {
		return fail(stderr, err)
	}
	exe, err := os.Executable()
	if err != nil {
		return fail(stderr, err)
	}
	args := []string{"serve", "--detached", "--data-dir", l.Root}
	if o.Port != 0 {
		args = append(args, "--port", strconv.Itoa(o.Port))
	}
	if o.LogLevel != "" {
		args = append(args, "--log-level", o.LogLevel)
	}
	logf, err := os.OpenFile(l.StartLog, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(stderr, err)
	}
	defer func() { _ = logf.Close() }() // the child has its own copy
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fail(stderr, err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.Now().Add(startTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			fmt.Fprintf(stderr, "meko: the server stopped while starting.\n%s", startLogTail(l))
			return exitError
		case <-time.After(100 * time.Millisecond):
		}
		if c, err := newClient(l); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			st, err := c.status(ctx)
			cancel()
			if err == nil && st.PID == cmd.Process.Pid {
				fmt.Fprintf(stdout, "Meko is running at http://127.0.0.1:%d (pid %d, data in %s).\n", st.Port, st.PID, l.Root)
				return exitOK
			}
		}
	}
	// Stop it for real before saying so: a retry right away must not find it still running.
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(stopTimeout):
		_ = cmd.Process.Kill()
		<-exited
	}
	fmt.Fprintf(stderr, "meko: the server didn't answer within %s, so it was stopped. See %s.\n%s",
		startTimeout, l.Log, startLogTail(l))
	return exitError
}

// startLogTail is the end of logs/start.log, indented, or "" if it's empty.
func startLogTail(l paths.Layout) string {
	b, err := os.ReadFile(l.StartLog)
	b = bytes.TrimSpace(b)
	if err != nil || len(b) == 0 {
		return ""
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return "  " + strings.Join(lines, "\n  ") + "\n"
}

// runStop sends SIGTERM to the server, waits for it to release the lock, and kills it if it
// takes longer than stopTimeout.
func runStop(l paths.Layout, stdout, stderr io.Writer) int {
	up, err := running(l.Lock)
	if err != nil {
		return fail(stderr, err)
	}
	if !up {
		cleanRunFiles(l) // left behind if the server was killed
		fmt.Fprintln(stdout, "Meko isn't running.")
		return exitOK
	}
	pid, err := readInt(l.Pid)
	if err != nil {
		return fail(stderr, fmt.Errorf(
			"the server is running, but its pid is unknown (%v). Stop the meko serve process yourself", err))
	}
	if err = syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fail(stderr, fmt.Errorf("can't stop pid %d: %w", pid, err))
	}
	if waitUnlocked(l.Lock, stopTimeout) {
		fmt.Fprintln(stdout, "Meko stopped.")
		return exitOK
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	if !waitUnlocked(l.Lock, 5*time.Second) {
		return fail(stderr, fmt.Errorf("pid %d didn't stop, even after SIGKILL", pid))
	}
	cleanRunFiles(l) // and its children, in process groups the SIGKILL didn't reach
	fmt.Fprintf(stdout, "Meko didn't stop within %s, so it was killed.\n", stopTimeout)
	return exitOK
}

func waitUnlocked(lock string, d time.Duration) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if up, err := running(lock); err == nil && !up {
			return true
		}
	}
	return false
}

func runStatus(l paths.Layout, stdout, stderr io.Writer) int {
	up, err := running(l.Lock)
	if err != nil {
		return fail(stderr, err)
	}
	if !up {
		fmt.Fprintf(stdout, "Meko isn't running (data in %s).\n", l.Root)
		return exitNotRunning
	}
	c, err := newClient(l)
	if err != nil {
		return fail(stderr, fmt.Errorf("the server is running, but %w", err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := c.status(ctx)
	if err != nil {
		return fail(stderr, fmt.Errorf("the server is running, but isn't answering: %w", err))
	}
	fmt.Fprintf(stdout, "Meko is running.\n")
	fmt.Fprintf(stdout, "  URL:      http://127.0.0.1:%d\n", st.Port)
	fmt.Fprintf(stdout, "  Version:  %s\n", st.Version)
	fmt.Fprintf(stdout, "  PID:      %d\n", st.PID)
	fmt.Fprintf(stdout, "  Uptime:   %s\n", (time.Duration(st.UptimeSeconds) * time.Second).String())
	fmt.Fprintf(stdout, "  Data:     %s\n", st.DataDir)
	engineLine := "  Engine:   " + st.Engine.State
	if e := st.Engine.Embedder; e != nil && e.Model != "" {
		engineLine += fmt.Sprintf(", %s %s (%d dims)", e.Provider, e.Model, e.Dims)
	}
	if st.Engine.Reason != "" {
		engineLine += " (" + st.Engine.Reason + ")"
	}
	fmt.Fprintln(stdout, engineLine)
	if st.Auth == config.AuthNone {
		fmt.Fprintln(stdout, "  Auth:     none (every user and program on this machine can read and write your memories)")
	} else {
		fmt.Fprintln(stdout, "  Auth:     token (meko token prints it)")
	}
	if len(st.Children) == 0 {
		fmt.Fprintf(stdout, "  Children: none\n")
	}
	for _, ch := range st.Children {
		line := fmt.Sprintf("  Child:    %s %s, %d restarts", ch.Name, ch.State, ch.Restarts)
		if ch.LastError != "" {
			line += " (" + ch.LastError + ")"
		}
		fmt.Fprintln(stdout, line)
	}
	return exitOK
}

// runToken prints the client token, making it if no server has yet. It reads the data folder, so
// it works whether or not the server runs. While one runs it only reads: the server keeps the token
// it loaded at start, so a token made now (after the file was deleted to replace it) would be one
// the server refuses.
func runToken(l paths.Layout, stdout, stderr io.Writer) int {
	if err := l.Ensure(); err != nil {
		return fail(stderr, err)
	}
	up, err := running(l.Lock)
	if err != nil {
		return fail(stderr, err)
	}
	var tok string
	if up {
		tok, err = auth.ReadClientToken(l.ClientToken)
		if errors.Is(err, fs.ErrNotExist) {
			err = fmt.Errorf("%s is gone, but the running server still takes the token it loaded at start. "+
				"Restart Meko (meko stop, then meko start) to make a new one", l.ClientToken)
		}
	} else {
		tok, err = auth.EnsureClientToken(l.ClientToken)
	}
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, tok)
	return exitOK
}

func runVersion(stdout, stderr io.Writer) int {
	v, err := store.LibVersions()
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "meko %s (commit %s)\n", version.Version, version.Commit)
	fmt.Fprintf(stdout, "  Go          %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(stdout, "  SQLite      %s\n", v.SQLite)
	fmt.Fprintf(stdout, "  sqlite-vec  %s\n", v.SQLiteVec)
	return exitOK
}
