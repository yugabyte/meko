package process

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Backoff controls how long the supervisor waits before restarting a process
// that exited on its own. The zero value yields sensible defaults.
type Backoff struct {
	Initial    time.Duration // delay before the first restart; zero means 1s
	Max        time.Duration // ceiling on the delay; zero means 30s
	ResetAfter time.Duration // a run lasting at least this long resets the delay to Initial; zero means 1m
}

func (b Backoff) initial() time.Duration {
	if b.Initial > 0 {
		return b.Initial
	}
	return time.Second
}

func (b Backoff) max() time.Duration {
	if b.Max > 0 {
		return b.Max
	}
	return 30 * time.Second
}

func (b Backoff) resetAfter() time.Duration {
	if b.ResetAfter > 0 {
		return b.ResetAfter
	}
	return time.Minute
}

// next returns the delay following one of duration prev (prev <= 0 means this
// is the first restart). It doubles, capped at Max.
func (b Backoff) next(prev time.Duration) time.Duration {
	if prev <= 0 {
		return b.initial()
	}
	return min(prev*2, b.max())
}

// Supervisor keeps a single child process running for the lifetime of a Run
// call, restarting it with backoff whenever it exits unexpectedly.
type Supervisor struct {
	spec   Spec
	logger *slog.Logger

	readyOnce sync.Once
	readyCh   chan struct{}
}

// NewSupervisor builds a supervisor for spec, logging under a logger scoped to
// the process name.
func NewSupervisor(spec Spec, logger *slog.Logger) *Supervisor {
	return &Supervisor{
		spec:    spec,
		logger:  logger.With("process", spec.Name),
		readyCh: make(chan struct{}),
	}
}

// Ready returns a channel closed the first time the process's readiness probe
// passes. If the spec has no Ready probe, the channel is never closed.
func (s *Supervisor) Ready() <-chan struct{} { return s.readyCh }

// Run starts the process and keeps it running — restarting it with backoff
// whenever it exits on its own — until ctx is cancelled. On cancellation it
// asks the current process to stop (StopSignal, escalating to SIGKILL after
// StopTimeout) and returns once it has exited.
func (s *Supervisor) Run(ctx context.Context) {
	// Log the path and an arg count, never the args themselves: a managed
	// process's args can carry secrets (e.g. the controller's
	// --database-url), and the controller never logs the universe connection
	// string elsewhere.
	s.logger.Info("supervising managed process", "path", s.spec.Path, "arg_count", len(s.spec.Args))

	// Own the readiness probe's lifetime so Run doesn't return while it is still
	// in flight: track it and wait for it before returning.
	var probeWG sync.WaitGroup
	if s.spec.Ready != nil {
		probeWG.Go(func() { s.probeReady(ctx) })
	}
	defer probeWG.Wait()

	var delay time.Duration
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		err := s.runOnce(ctx)
		ran := time.Since(start)

		if ctx.Err() != nil {
			// Shutdown: any error here is just the stop we initiated.
			s.logger.Info("managed process stopped", "ran", ran.String())
			return
		}
		if err != nil {
			s.logger.Error("managed process exited", "error", err, "ran", ran.String())
		} else {
			s.logger.Warn("managed process exited unexpectedly", "ran", ran.String())
		}

		// A process that stayed up long enough is considered healthy: forget
		// the accumulated backoff so an unrelated later crash restarts promptly.
		if ran >= s.spec.Restart.resetAfter() {
			delay = 0
		}
		delay = s.spec.Restart.next(delay)
		s.logger.Info("restarting managed process", "delay", delay.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// runOnce runs the process to completion once, returning its exit error (nil on
// a clean exit). When ctx is cancelled it sends StopSignal; WaitDelay escalates
// to SIGKILL if the process does not exit within StopTimeout.
func (s *Supervisor) runOnce(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, s.spec.Path, s.spec.Args...)
	cmd.Dir = s.spec.Dir
	if len(s.spec.Env) > 0 {
		cmd.Env = append(os.Environ(), s.spec.Env...)
	}

	stdout := &logWriter{logger: s.logger, stream: "stdout"}
	stderr := &logWriter{logger: s.logger, stream: "stderr"}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	sig := s.spec.stopSignal()
	cmd.Cancel = func() error { return cmd.Process.Signal(sig) }
	cmd.WaitDelay = s.spec.stopTimeout()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	s.logger.Info("managed process started", "pid", cmd.Process.Pid)

	err := cmd.Wait()
	// Wait has joined the stdout/stderr copy goroutines, so flushing the
	// trailing partial lines here cannot race with a Write.
	stdout.flush()
	stderr.flush()
	return err
}

// probeReady polls Ready until it first succeeds (or ctx ends), then closes the
// ready channel. It runs for the whole Run lifetime, independent of restarts.
func (s *Supervisor) probeReady(ctx context.Context) {
	t := time.NewTicker(s.spec.readyInterval())
	defer t.Stop()

	var deadline <-chan time.Time // nil (blocks forever) when no ReadyTimeout
	if s.spec.ReadyTimeout > 0 {
		timer := time.NewTimer(s.spec.ReadyTimeout)
		defer timer.Stop()
		deadline = timer.C
	}

	for {
		if err := s.spec.Ready(ctx); err == nil {
			s.readyOnce.Do(func() {
				close(s.readyCh)
				s.logger.Info("managed process ready")
			})
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			s.logger.Warn("managed process not ready within timeout", "timeout", s.spec.ReadyTimeout.String())
			return
		case <-t.C:
		}
	}
}
