package update

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	Error     string    `json:"error,omitempty"`
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

// Updater checks for and applies Meko's releases from inside serve.
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
	// Restart ends serve cleanly once a new version is current; the service manager starts it.
	Restart func()
	// FirstCheck is how long after start the first check runs.
	FirstCheck time.Duration
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
		if p, _ := u.App.ReadPending(); p != nil {
			continue // a new version is on probation: no further update until it's done
		}
		rel, ver, newer, err := u.Check(ctx)
		if err != nil || !newer {
			continue
		}
		if !u.Auto {
			u.Log.Info("an update is available; meko update installs it", "version", ver)
			continue
		}
		u.Log.Info("installing an update", "from", u.Version, "to", ver)
		if err := u.Apply(ctx, rel, ver); err != nil {
			u.Log.Error("the update failed; this version keeps running", "version", ver, "error", err)
			continue
		}
		u.Log.Info("restarting into the update", "version", ver)
		u.Restart()
		return
	}
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
	goos, arch := Platform()
	rel, ver, ok := PickMeko(rels, goos, arch, u.App.BadVersions())
	if !ok {
		return Release{}, "", false, nil
	}
	st.Latest = ver
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
