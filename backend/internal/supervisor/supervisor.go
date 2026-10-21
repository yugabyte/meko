// Package supervisor runs Meko's child processes (the Node engine from milestone 2,
// connectors later): it starts each one in its own process group, waits for it to be healthy,
// restarts it with backoff when it exits, gives up after repeated crashes, and stops the whole
// group on shutdown.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Spec describes one child. Zero durations and counts take the defaults noted.
type Spec struct {
	Name    string
	Path    string
	Args    []string
	Env     []string // added to this process's environment
	Dir     string
	LogFile string // stdout and stderr, appended
	// PidFile, if set, holds the child's Record while it runs, so stop can still reach its
	// process group after killing a serve that hung (KillRecorded).
	PidFile string

	// Health reports whether the child is ready. Nil means ready as soon as it starts.
	Health       func(ctx context.Context) error
	StartTimeout time.Duration // to become healthy; default 30 s

	MinBackoff  time.Duration // first restart delay; default 1 s, doubling
	MaxBackoff  time.Duration // default 60 s
	StableAfter time.Duration // running this long resets the delay; default 60 s
	MaxCrashes  int           // within CrashWindow, then the child is failed; default 5
	CrashWindow time.Duration // default 10 min
	StopTimeout time.Duration // SIGTERM to SIGKILL; default 10 s
}

func (s Spec) withDefaults() Spec {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&s.StartTimeout, 30*time.Second)
	def(&s.MinBackoff, time.Second)
	def(&s.MaxBackoff, time.Minute)
	def(&s.StableAfter, time.Minute)
	def(&s.CrashWindow, 10*time.Minute)
	def(&s.StopTimeout, 10*time.Second)
	if s.MaxCrashes == 0 {
		s.MaxCrashes = 5
	}
	return s
}

// State is where a child is in its life.
type State string

// States.
const (
	Starting State = "starting"
	Running  State = "running"
	Backoff  State = "backoff" // waiting to restart after a crash
	Failed   State = "failed"  // crashed too often; not restarted
	Stopped  State = "stopped"
)

// Status is a child's state for status reports.
type Status struct {
	Name      string    `json:"name"`
	State     State     `json:"state"`
	PID       int       `json:"pid,omitempty"`
	Restarts  int       `json:"restarts"`
	LastError string    `json:"last_error,omitempty"`
	Since     time.Time `json:"since"`
}

// Supervisor owns the children.
type Supervisor struct {
	log      *slog.Logger
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.Mutex
	children []*child
}

// New returns a supervisor with no children.
func New(log *slog.Logger) *Supervisor {
	ctx, cancel := context.WithCancel(context.Background())
	return &Supervisor{log: log, ctx: ctx, cancel: cancel}
}

// Start runs a child under supervision.
func (s *Supervisor) Start(spec Spec) {
	c := &child{spec: spec.withDefaults(), log: s.log.With("child", spec.Name)}
	c.set(Starting, 0, "")
	s.mu.Lock()
	s.children = append(s.children, c)
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		c.run(s.ctx)
	}()
}

// Statuses reports every child, in the order they were started.
func (s *Supervisor) Statuses() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Status, len(s.children))
	for i, c := range s.children {
		out[i] = c.status()
	}
	return out
}

// Stop stops every child (SIGTERM to its group, then SIGKILL after its StopTimeout) and waits
// for them, until ctx ends.
func (s *Supervisor) Stop(ctx context.Context) error {
	s.cancel()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("children didn't stop in time: %w", ctx.Err())
	}
}

type child struct {
	spec    Spec
	log     *slog.Logger
	mu      sync.Mutex
	st      Status
	crashes []time.Time
}

func (c *child) set(state State, pid int, lastErr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.st.Name, c.st.State, c.st.PID, c.st.Since = c.spec.Name, state, pid, time.Now()
	if lastErr != "" {
		c.st.LastError = lastErr
	}
}

func (c *child) status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st
}

func (c *child) run(ctx context.Context) {
	delay := c.spec.MinBackoff
	for {
		ranFor, err := c.once(ctx)
		if ctx.Err() != nil {
			c.set(Stopped, 0, "")
			return
		}
		now := time.Now()
		c.crashes = append(c.crashes, now)
		for len(c.crashes) > 0 && now.Sub(c.crashes[0]) > c.spec.CrashWindow {
			c.crashes = c.crashes[1:]
		}
		if len(c.crashes) >= c.spec.MaxCrashes {
			c.log.Error("child failed: too many crashes; not restarting it",
				"crashes", len(c.crashes), "window", c.spec.CrashWindow, "error", err)
			c.set(Failed, 0, err.Error())
			return
		}
		if ranFor >= c.spec.StableAfter {
			delay = c.spec.MinBackoff
		}
		c.log.Warn("child exited; restarting", "error", err, "in", delay)
		c.set(Backoff, 0, err.Error())
		select {
		case <-ctx.Done():
			c.set(Stopped, 0, "")
			return
		case <-time.After(delay):
		}
		c.mu.Lock()
		c.st.Restarts++ // only restarts made: the crash that fails the child isn't one
		c.mu.Unlock()
		delay = min(delay*2, c.spec.MaxBackoff)
	}
}

// once runs the child until it exits or ctx ends, and reports how long it was healthy and why it
// stopped.
func (c *child) once(ctx context.Context) (time.Duration, error) {
	logf, err := os.OpenFile(c.spec.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer func() { _ = logf.Close() }()              // the child has its own copy
	cmd := exec.Command(c.spec.Path, c.spec.Args...) //nolint:gosec // G204: specs come from code, not input
	cmd.Env = append(os.Environ(), c.spec.Env...)
	cmd.Dir = c.spec.Dir
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("can't start %s: %w", c.spec.Path, err)
	}
	pid := cmd.Process.Pid
	c.set(Starting, pid, "")
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	if c.spec.PidFile != "" {
		rec, err := Record(pid)
		if err == nil {
			err = writeFile(c.spec.PidFile, rec)
		}
		if err != nil {
			c.log.Warn("can't record the child's pid; stop can't reach it if serve hangs", "error", err)
		}
		defer func() { _ = os.Remove(c.spec.PidFile) }()
	}

	if err := c.waitHealthy(ctx, exited); err != nil {
		c.stopGroup(pid, exited)
		return 0, err
	}
	if ctx.Err() != nil {
		c.stopGroup(pid, exited)
		return 0, nil
	}
	c.log.Info("child running", "pid", pid)
	c.set(Running, pid, "")
	since := time.Now()
	select {
	case err := <-exited:
		// It went on its own, but anything it started in its group may still run (holding a
		// port, say): sweep the group before it's restarted. It's no longer running meanwhile.
		ran := time.Since(since)
		c.set(Backoff, 0, exitError(err).Error())
		exited <- err
		c.stopGroup(pid, exited)
		return ran, exitError(err)
	case <-ctx.Done():
		c.stopGroup(pid, exited)
		return time.Since(since), nil
	}
}

func (c *child) waitHealthy(ctx context.Context, exited chan error) error {
	if c.spec.Health == nil {
		return nil
	}
	deadline := time.Now().Add(c.spec.StartTimeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		hctx, cancel := context.WithTimeout(ctx, time.Second)
		err := c.spec.Health(hctx)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case e := <-exited:
			exited <- e // stopGroup reads it again
			return exitError(e)
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not healthy within %s: %v", c.spec.StartTimeout, err)
		}
	}
}

// stopGroup sends SIGTERM to the child's process group and waits for the child to exit. Anything
// still in the group at StopTimeout gets SIGKILL, even when the child itself exited first: its own
// children (a worker, say) may ignore SIGTERM.
func (c *child) stopGroup(pid int, exited chan error) {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.After(c.spec.StopTimeout)
	select {
	case <-exited:
	case <-deadline:
		c.log.Warn("child didn't stop after SIGTERM; killing its process group", "pid", pid)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-exited
		return
	}
	for groupAlive(pid) {
		select {
		case <-deadline:
			c.log.Warn("processes left in the child's group after SIGTERM; killing them", "pgid", pid)
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// groupAlive reports whether any process is left in the process group.
func groupAlive(pgid int) bool { return syscall.Kill(-pgid, 0) == nil }

func exitError(err error) error {
	if err == nil {
		return errors.New("exited with status 0")
	}
	return fmt.Errorf("exited: %w", err)
}

func writeFile(path, data string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(data), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Record is what PidFile holds for a child that leads its own process group (Setpgid): its pid,
// its pgid (the same) and when it started, "<pid> <pgid> <start>".
func Record(pid int) (string, error) {
	start, err := processStart(pid)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d %d %s\n", pid, pid, start), nil
}

// KillRecorded sends SIGKILL to the process group of each child recorded in dir (PidFile) and
// removes the records. A group is killed only while its leader still has the recorded pgid and
// started when the record says, so a pid reused since (after a crash or a reboot) is never hit.
// It's for after serve itself was killed, or before a new serve starts.
func KillRecorded(dir string) (killed int, err error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(path) //nolint:gosec // G304: our own run folder
		if err != nil {
			continue
		}
		if pgid, ok := liveGroup(string(b)); ok && syscall.Kill(-pgid, syscall.SIGKILL) == nil {
			killed++
		}
		_ = os.Remove(path)
	}
	return killed, nil
}

// liveGroup is the process group a Record names, if its leader is still the process recorded.
func liveGroup(rec string) (int, bool) {
	var pid, pgid int
	var start string
	if _, err := fmt.Sscanf(rec, "%d %d %s", &pid, &pgid, &start); err != nil || pid <= 1 || pgid <= 1 {
		return 0, false
	}
	if got, err := syscall.Getpgid(pid); err != nil || got != pgid {
		return 0, false
	}
	now, err := processStart(pid)
	return pgid, err == nil && now == start
}

// UnixHTTPCheck is a Health function that GETs path over the unix socket and wants a 2xx.
func UnixHTTPCheck(socket, path string) func(ctx context.Context) error {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix"+path, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("health check answered %s", resp.Status)
		}
		return nil
	}
}
