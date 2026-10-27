package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/yugabyte/meko/backend/internal/paths"
)

// Channels.
const (
	ChannelStable = "stable"
	ChannelOff    = "off"
)

// State is what the last check found, in update.json in the data folder; meko status shows it.
type State struct {
	CheckedAt time.Time `json:"checked_at"`
	Running   string    `json:"running"`
	Latest    string    `json:"latest,omitempty"`
	LatestURL string    `json:"latest_url,omitempty"` // the release's page on GitHub
	Error     string    `json:"error,omitempty"`
	// Extensions are what the same check found for each extension Meko updates, by name.
	Extensions map[string]ExtensionState `json:"extensions,omitempty"`
}

// ExtensionState is the newest release of an extension the last check found (its bad versions
// left out); "" when it found none for this platform.
type ExtensionState struct {
	Latest    string `json:"latest,omitempty"`
	LatestURL string `json:"latest_url,omitempty"`
}

// StateFile is where the data folder at root keeps State.
func StateFile(root string) string { return filepath.Join(root, "update.json") }

// ReadState reads State; ok is false if there's none.
func ReadState(root string) (State, bool) {
	var s State
	b, err := os.ReadFile(StateFile(root)) //nolint:gosec // G304: the data folder's own file
	if err != nil || json.Unmarshal(b, &s) != nil {
		return State{}, false
	}
	return s, true
}

// Updater checks for and applies Meko's releases from inside serve, and has the same check find the
// extensions' (Extensions) and install the ones set to update on their own (AfterCheck).
type Updater struct {
	App      App
	Version  string // the running version
	Client   *Client
	DataDir  string
	Interval time.Duration
	// Auto applies an update when one is found; otherwise updates are only reported (status).
	Auto bool
	// Schema is the running binary's database schema, recorded for a rollback.
	Schema int
	Log    *slog.Logger
	// Restart ends serve cleanly once a new version is current; the service manager starts it, or
	// without one serve runs it in its own place.
	Restart func()
	// FirstCheck is how long after start the first check runs.
	FirstCheck time.Duration
	// Channel is [update] channel; Service is whether the login service runs this server. The web
	// UI's Status shows both.
	Channel string
	Service bool
	// Context is what an install the web UI started runs with: serve's, which ends as it stops.
	Context context.Context
	// ExtensionsOnly is an updater for a Meko built from source, which never updates itself: its
	// checks look only for the extensions' releases.
	ExtensionsOnly bool
	// Extensions, if set, reads each extension's newest release from the releases a check listed;
	// what it returns is recorded with the check (State.Extensions).
	Extensions func(releases []Release) map[string]ExtensionState
	// AfterCheck, if set, runs after a scheduled check that doesn't restart into a new Meko, with
	// Lock held: it installs the extensions' updates that install on their own.
	AfterCheck func(ctx context.Context)
	// Lock is held by a check or an install, the scheduled ones and the web UI's, Meko's and the
	// extensions', so two never stage at once; after a switch to a new Meko it stays held until
	// serve restarts. Nil is a lock of its own on DataDir.
	Lock *Lock

	lockOnce sync.Once
	jobMu    sync.Mutex
	job      *Job
}

func (u *Updater) lock() *Lock {
	u.lockOnce.Do(func() {
		if u.Lock == nil {
			u.Lock = NewLock(u.DataDir)
		}
	})
	return u.Lock
}

// The states of a Job.
const (
	JobRunning = "running"
	JobDone    = "done"
	JobFailed  = "failed"
)

// The steps of a running Job.
const (
	StepChecking   = "checking"   // listing the releases
	StepInstalling = "installing" // downloading, checking and unpacking the release, then switching to it
	StepRestarting = "restarting" // serve is stopping to start the new version
)

// Job is the last install the web UI started since the server started. To is the version it
// installs, "" until the check has found one, and still "" when it ends done with nothing newer.
type Job struct {
	State      string     `json:"state"`
	Step       string     `json:"step,omitempty"`
	To         string     `json:"to,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// The reasons StartApply and CheckNow refuse.
var (
	ErrBusy    = errors.New("an update check or install is already running")
	ErrPending = errors.New("the last update hasn't finished: a new version counts as installed once it has run 5 minutes")
	ErrOff     = errors.New("updates are off")
)

// Off is the web UI's view of a release install whose updater couldn't be set up: Status says why,
// and checks and installs are refused (ErrOff).
type Off struct{ Version, Reason string }

func (o Off) Status() Status {
	return Status{Running: o.Version, Release: true, Off: o.Reason, RolledBack: []string{}}
}

func (o Off) CheckNow(context.Context) (Status, error) { return o.Status(), ErrOff }

func (o Off) StartApply() (Status, error) { return o.Status(), ErrOff }

// Status is what the web UI's Status page shows about updates.
type Status struct {
	Running string `json:"running"`
	// Release is whether this is a release install, which updates itself; a build from source doesn't.
	Release bool `json:"release"`
	// Off is why a release install's updates can't run (Off); "" when they can.
	Off     string `json:"off,omitempty"`
	Service bool   `json:"service"`
	Channel string `json:"channel,omitempty"`
	// Auto is whether this server installs updates on its own: under the service, with [update] auto.
	Auto      bool       `json:"auto"`
	CheckedAt *time.Time `json:"checked_at,omitempty"`
	Latest    string     `json:"latest,omitempty"`
	LatestURL string     `json:"latest_url,omitempty"`
	// Available is whether Latest is newer than the running version and wasn't rolled back.
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"`
	// Pending is an update on probation.
	Pending *Pending `json:"pending,omitempty"`
	// RolledBack are the versions rolled back (bad-versions), never offered again.
	RolledBack []string `json:"rolled_back"`
	Job        *Job     `json:"job,omitempty"`
}

// Run checks at FirstCheck and every Interval until ctx ends, applying an update when Auto.
func (u *Updater) Run(ctx context.Context) {
	wait := u.FirstCheck
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = u.Interval
		// A new Meko on probation installs no further update of its own until it's done, but its
		// check still runs, for the extensions.
		onProbation := false
		if !u.ExtensionsOnly {
			p, _ := u.App.ReadPending()
			onProbation = p != nil
		}
		if !u.lock().TryLock() {
			continue // the web UI, or another meko, is checking or installing
		}
		if onProbation {
			_, _, _, _ = u.Check(ctx)
		} else if u.scheduled(ctx) {
			u.Log.Info("restarting into the update")
			u.Restart()
			return // the lock stays held: nothing else installs before the restart
		}
		if u.AfterCheck != nil {
			u.AfterCheck(ctx)
		}
		u.lock().Unlock()
	}
}

// scheduled is a scheduled check, installing what it finds when Auto. It reports whether a new
// version is now current. The caller holds the lock.
func (u *Updater) scheduled(ctx context.Context) bool {
	rel, ver, newer, err := u.Check(ctx)
	if err != nil || !newer {
		return false
	}
	if !u.Auto {
		u.Log.Info("an update is available; meko update installs it", "version", ver)
		return false
	}
	u.Log.Info("installing an update", "from", u.Version, "to", ver)
	if err := u.Apply(ctx, rel, ver); err != nil {
		u.Log.Error("the update failed; this version keeps running", "version", ver, "error", err)
		return false
	}
	return true
}

// Status is what the last check found, the update on probation, the versions rolled back and the
// web UI's last install.
func (u *Updater) Status() Status {
	s := Status{Running: u.Version, Release: true, Service: u.Service, Channel: u.Channel, Auto: u.Auto,
		RolledBack: []string{}}
	bad := u.App.BadVersions()
	if st, ok := ReadState(u.DataDir); ok {
		s.CheckedAt, s.Latest, s.LatestURL, s.Error = &st.CheckedAt, st.Latest, st.LatestURL, st.Error
		cur, _ := ParseVersion(u.Version)
		// update.json can predate a rollback, and still name the version rolled back.
		if v, ok := ParseVersion(st.Latest); ok && !bad[st.Latest] {
			s.Available = cur.Less(v)
		}
	}
	s.Pending, _ = u.App.ReadPending()
	for v := range bad {
		s.RolledBack = append(s.RolledBack, v)
	}
	sort.Strings(s.RolledBack)
	u.jobMu.Lock()
	if u.job != nil {
		j := *u.job
		s.Job = &j
	}
	u.jobMu.Unlock()
	return s
}

// CheckNow checks for a release now, for the web UI, unless a check or an install is running
// (ErrBusy). A failed check isn't an error here: Status says why.
func (u *Updater) CheckNow(ctx context.Context) (Status, error) {
	if !u.lock().TryLock() {
		return u.Status(), ErrBusy
	}
	defer u.lock().Unlock()
	_, _, _, _ = u.Check(ctx)
	return u.Status(), nil
}

// StartApply installs the newest release for the web UI, in the background with Context, and
// restarts into it. It refuses while an update is on probation (ErrPending) or while a check or an
// install is running (ErrBusy). Status's Job follows it.
func (u *Updater) StartApply() (Status, error) {
	if p, _ := u.App.ReadPending(); p != nil {
		return u.Status(), ErrPending
	}
	if !u.lock().TryLock() {
		return u.Status(), ErrBusy
	}
	u.setJob(func(j *Job) { *j = Job{State: JobRunning, Step: StepChecking, StartedAt: time.Now().UTC()} })
	go u.apply()
	return u.Status(), nil
}

// apply is StartApply's install. It holds the lock, and keeps it once a new version is current.
func (u *Updater) apply() {
	ctx := u.Context
	if ctx == nil {
		ctx = context.Background()
	}
	err := func() error {
		rel, ver, newer, err := u.Check(ctx)
		if err != nil {
			return err
		}
		if !newer {
			return nil
		}
		u.setJob(func(j *Job) { j.Step, j.To = StepInstalling, ver })
		u.Log.Info("installing an update from the web UI", "from", u.Version, "to", ver)
		if err := u.Apply(ctx, rel, ver); err != nil {
			return err
		}
		u.setJob(func(j *Job) { j.Step = StepRestarting })
		return nil
	}()
	j := u.jobView()
	if err == nil && j.Step == StepRestarting {
		u.Log.Info("restarting into the update", "version", j.To)
		u.Restart()
		return
	}
	defer u.lock().Unlock()
	u.setJob(func(j *Job) {
		now := time.Now().UTC()
		j.State, j.Step, j.FinishedAt = JobDone, "", &now
		if err != nil {
			j.State, j.Error = JobFailed, err.Error()
		}
	})
	if err != nil {
		u.Log.Error("the update failed; this version keeps running", "version", j.To, "error", err)
	}
}

func (u *Updater) setJob(f func(*Job)) {
	u.jobMu.Lock()
	defer u.jobMu.Unlock()
	if u.job == nil {
		u.job = &Job{}
	}
	f(u.job)
}

func (u *Updater) jobView() Job {
	u.jobMu.Lock()
	defer u.jobMu.Unlock()
	return *u.job
}

// Check looks for a release newer than the running one and records what it found.
func (u *Updater) Check(ctx context.Context) (Release, string, bool, error) {
	st := State{CheckedAt: time.Now().UTC(), Running: u.Version}
	defer func() {
		if b, err := json.Marshal(st); err == nil {
			_ = paths.WriteFile(StateFile(u.DataDir), b)
		}
	}()
	rels, err := u.Client.Releases(ctx)
	if err != nil {
		st.Error = err.Error()
		u.Log.Warn("can't check for updates", "error", err)
		return Release{}, "", false, err
	}
	if u.Extensions != nil {
		st.Extensions = u.Extensions(rels)
	}
	if u.ExtensionsOnly {
		return Release{}, "", false, nil
	}
	goos, arch := Platform()
	rel, ver, ok := PickMeko(rels, goos, arch, u.App.BadVersions())
	if !ok {
		return Release{}, "", false, nil
	}
	st.Latest, st.LatestURL = ver, rel.URL
	cur, _ := ParseVersion(u.Version)
	v, _ := ParseVersion(ver)
	return rel, ver, cur.Less(v), nil
}

// Apply stages a release and makes it current.
func (u *Updater) Apply(ctx context.Context, rel Release, ver string) error {
	if err := u.App.Stage(ctx, u.Client, rel, ver); err != nil {
		return err
	}
	if err := u.App.Switch(u.Version, ver, u.Schema); err != nil {
		return fmt.Errorf("can't switch to %s: %w", ver, err)
	}
	return nil
}
