package server

import (
	"bytes"
	"context"
	"sort"
	"sync"
	"time"
)

// An extension's action the web UI started (install, start or stop) runs in the background, since
// an install takes minutes: one at a time per extension, kept in memory until the next one, with the
// end of its output. GET /api/v1/extensions shows it, and the page polls that while one runs.

// The states of an ExtensionJob.
const (
	JobRunning = "running"
	JobDone    = "done"
	JobFailed  = "failed"
)

// JobOutputLimit is how much of a job's output it keeps: the last 16 KiB.
const JobOutputLimit = 16 << 10

// ExtensionJob is the last action on an extension since the server started.
type ExtensionJob struct {
	Action     string     `json:"action"`
	State      string     `json:"state"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      string     `json:"error,omitempty"`
	Output     string     `json:"output"`
}

// job is a running or finished ExtensionJob; its runner writes the output while the list reads it.
type job struct {
	mu   sync.Mutex
	view ExtensionJob
	out  []byte
}

// Write keeps the end of the output, from a line's start when it had to drop some.
func (j *job) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.out = append(j.out, p...)
	if over := len(j.out) - JobOutputLimit; over > 0 {
		rest := j.out[over:]
		if i := bytes.IndexByte(rest, '\n'); i >= 0 && i < len(rest)-1 {
			rest = rest[i+1:]
		}
		j.out = append([]byte(nil), rest...)
	}
	return len(p), nil
}

func (j *job) snapshot() ExtensionJob {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := j.view
	v.Output = string(j.out)
	return v
}

func (j *job) finish(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now().UTC()
	j.view.FinishedAt, j.view.State = &now, JobDone
	if err != nil {
		j.view.State, j.view.Error = JobFailed, err.Error()
	}
}

// ExtensionJobs are the extensions' jobs, by name. The web listener's owner keeps one for all the
// servers it builds (Web.Jobs), so a job outlives a restart of the listener; Cancel stops the
// running ones when Meko stops.
type ExtensionJobs struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	jobs    map[string]*job
	running int
	ended   chan struct{} // closed, and replaced, each time a job ends
}

// NewExtensionJobs returns an empty set of jobs.
func NewExtensionJobs() *ExtensionJobs {
	ctx, cancel := context.WithCancel(context.Background())
	return &ExtensionJobs{ctx: ctx, cancel: cancel, jobs: map[string]*job{}, ended: make(chan struct{})}
}

// start begins a job for name, unless one is running: then it returns that one and false. The
// caller runs it with run's context and ends it with done.
func (x *ExtensionJobs) start(name, action string) (*job, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if j := x.jobs[name]; j != nil && j.snapshot().State == JobRunning {
		return j, false
	}
	j := &job{view: ExtensionJob{Action: action, State: JobRunning, StartedAt: time.Now().UTC()}}
	x.jobs[name] = j
	x.running++
	return j, true
}

// run is the context a job runs with: done once Cancel is called.
func (x *ExtensionJobs) run() context.Context { return x.ctx }

func (x *ExtensionJobs) done(j *job, err error) {
	j.finish(err)
	x.mu.Lock()
	defer x.mu.Unlock()
	x.running--
	close(x.ended)
	x.ended = make(chan struct{})
}

// get is name's last job, or nil.
func (x *ExtensionJobs) get(name string) *ExtensionJob {
	x.mu.Lock()
	j := x.jobs[name]
	x.mu.Unlock()
	if j == nil {
		return nil
	}
	v := j.snapshot()
	return &v
}

// Busy reports whether a job is running.
func (x *ExtensionJobs) Busy() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.running > 0
}

// Running is a running job's extension and action; ok is false when none runs.
func (x *ExtensionJobs) Running() (name, action string, ok bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.running == 0 {
		return "", "", false
	}
	names := make([]string, 0, len(x.jobs))
	for n := range x.jobs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if v := x.jobs[n].snapshot(); v.State == JobRunning {
			return n, v.Action, true
		}
	}
	return "", "", false
}

// WaitIdle waits until no job is running, or ctx ends.
func (x *ExtensionJobs) WaitIdle(ctx context.Context) {
	for {
		x.mu.Lock()
		running, ended := x.running, x.ended
		x.mu.Unlock()
		if running == 0 {
			return
		}
		select {
		case <-ended:
		case <-ctx.Done():
			return
		}
	}
}

// Cancel stops the running jobs (each fails, "stopped") and any started later, and waits up to d
// for them to end.
func (x *ExtensionJobs) Cancel(d time.Duration) {
	x.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	x.WaitIdle(ctx)
}
