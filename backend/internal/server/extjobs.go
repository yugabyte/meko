package server

import (
	"bytes"
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

// extJobs are the extensions' jobs, by name.
type extJobs struct {
	mu   sync.Mutex
	jobs map[string]*job
}

// start begins a job for name, unless one is running: then it returns that one and false.
func (x *extJobs) start(name, action string) (*job, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if j := x.jobs[name]; j != nil && j.snapshot().State == JobRunning {
		return j, false
	}
	if x.jobs == nil {
		x.jobs = map[string]*job{}
	}
	j := &job{view: ExtensionJob{Action: action, State: JobRunning, StartedAt: time.Now().UTC()}}
	x.jobs[name] = j
	return j, true
}

// get is name's last job, or nil.
func (x *extJobs) get(name string) *ExtensionJob {
	x.mu.Lock()
	j := x.jobs[name]
	x.mu.Unlock()
	if j == nil {
		return nil
	}
	v := j.snapshot()
	return &v
}
