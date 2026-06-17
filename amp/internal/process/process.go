// Package process runs and supervises child processes that the amp controller
// fully owns: it starts them, streams their stdout/stderr into the controller's
// structured log, restarts them with backoff when they exit on their own, and
// stops them cleanly on shutdown. Its first user is the managed storage
// controller (yblight-controller.md §11.1), but a Manager supervises any number
// of processes (issue #94).
package process

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"syscall"
	"time"
)

// Spec describes a child process to supervise. The zero value is not usable;
// at least Name and Path must be set.
type Spec struct {
	// Name labels the process in logs (e.g. "storage_controller").
	Name string
	// Path is the executable to run. It is resolved against PATH when it has
	// no slash, matching os/exec.Command.
	Path string
	// Args are passed to the executable (excluding argv[0]).
	Args []string
	// Env holds extra "KEY=VALUE" entries appended to the controller's own
	// environment. Empty means inherit the controller's environment unchanged.
	Env []string
	// Dir is the working directory; empty means the controller's cwd.
	Dir string
	// Ready, if set, is polled after the process starts; the supervisor logs
	// and signals (via Supervisor.Ready) the first time it returns nil. It must
	// be cheap and must respect ctx.
	Ready func(ctx context.Context) error
	// ReadyInterval is how often Ready is polled; zero means 500ms.
	ReadyInterval time.Duration
	// ReadyTimeout, if set, bounds the wait for the first successful Ready: if
	// the probe has not passed within this long after Run starts, the
	// supervisor logs a warning and stops probing. The process keeps running
	// and restarting regardless — this only surfaces a slow/stuck startup.
	ReadyTimeout time.Duration
	// StopSignal is sent to ask the process to exit; zero means SIGTERM.
	StopSignal syscall.Signal
	// StopTimeout bounds the wait after StopSignal before the process is killed
	// (SIGKILL). Zero means 10s.
	StopTimeout time.Duration
	// Restart tunes the crash-restart backoff.
	Restart Backoff
}

func (s Spec) stopSignal() syscall.Signal {
	if s.StopSignal != 0 {
		return s.StopSignal
	}
	return syscall.SIGTERM
}

func (s Spec) stopTimeout() time.Duration {
	if s.StopTimeout > 0 {
		return s.StopTimeout
	}
	return 10 * time.Second
}

func (s Spec) readyInterval() time.Duration {
	if s.ReadyInterval > 0 {
		return s.ReadyInterval
	}
	return 500 * time.Millisecond
}

// logWriter turns a child's byte stream into one structured log record per
// line, buffering partial lines across writes. os/exec writes to it from a
// single goroutine per stream, so it needs no locking; flush is called only
// after Wait has joined that goroutine.
type logWriter struct {
	logger *slog.Logger
	stream string // "stdout" | "stderr"
	buf    bytes.Buffer
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	// Emit each complete line and consume it; leave any trailing partial line
	// buffered for the next write. Peeking with IndexByte and consuming via Next
	// avoids re-appending the partial line on every write (which the previous
	// ReadString approach did), so normal line-oriented output stays O(N).
	for {
		b := w.buf.Bytes()
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			break
		}
		w.emit(string(b[:i+1]))
		w.buf.Next(i + 1)
	}
	return len(p), nil
}

// flush emits any buffered partial line. Call it after the process exits and
// Wait has returned, so there is no concurrent Write.
func (w *logWriter) flush() {
	if w.buf.Len() > 0 {
		w.emit(w.buf.String())
		w.buf.Reset()
	}
}

func (w *logWriter) emit(line string) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return
	}
	w.logger.LogAttrs(context.Background(), slog.LevelInfo, "child output",
		slog.String("stream", w.stream), slog.String("line", line))
}
