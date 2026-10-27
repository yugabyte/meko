package update

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/yugabyte/meko/backend/internal/paths"
)

// MaxFailedStarts is how many times a new version may fail to start before it's rolled back.
const MaxFailedStarts = 3

// HealthyAfter is how long a new version must run before its update counts as done.
var HealthyAfter = 5 * time.Minute

// Pending is update-pending.json: an update that has switched app/current but hasn't yet run
// HealthyAfter. Schema is the old version's database schema, so a rollback can put back the copy
// the new version's migration made.
type Pending struct {
	From     string    `json:"from"`
	To       string    `json:"to"`
	Schema   int       `json:"schema"`
	Attempts int       `json:"attempts"` // starts of To, counting the one in progress
	At       time.Time `json:"at"`
}

func (a App) pendingFile() string { return filepath.Join(a.Root, "update-pending.json") }

// ReadPending returns the pending update, or nil. A record in the legacy folder (one an extension's
// own updater wrote, before Meko kept its records) is moved into the app folder first.
func (a App) ReadPending() (*Pending, error) {
	if err := a.importLegacyPending(); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(a.pendingFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Pending
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, nil //nolint:nilerr // a damaged file is no pending update; the next one rewrites it
	}
	return &p, nil
}

// importLegacyPending moves a legacy update-pending.json into the app folder, unless one is there:
// AgentK's {from, to, attempts, at} with at in milliseconds. A damaged one is dropped.
func (a App) importLegacyPending() error {
	if a.Legacy == "" {
		return nil
	}
	old := filepath.Join(a.Legacy, "update-pending.json")
	b, err := os.ReadFile(old) //nolint:gosec // G304: the extension's own folder
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, serr := os.Stat(a.pendingFile()); serr != nil {
		var l struct {
			From     string          `json:"from"`
			To       string          `json:"to"`
			Attempts int             `json:"attempts"`
			At       json.RawMessage `json:"at"`
		}
		if json.Unmarshal(b, &l) == nil && l.To != "" {
			p := Pending{From: l.From, To: l.To, Attempts: l.Attempts, At: time.Now().UTC()}
			var ms int64
			if json.Unmarshal(l.At, &ms) == nil && ms > 0 {
				p.At = time.UnixMilli(ms).UTC()
			}
			if err := a.writePending(p); err != nil {
				return err
			}
		}
	}
	return paths.RemoveIfExists(old)
}

func (a App) writePending(p Pending) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return paths.WriteFile(a.pendingFile(), b)
}

// ClearPending forgets the pending update (a finished one, or a manual install over it).
func (a App) ClearPending() error { return paths.RemoveIfExists(a.pendingFile()) }

// Switch makes version current, recording the update so a failing new version is rolled back.
func (a App) Switch(from, to string, schema int) error {
	if err := a.writePending(Pending{From: from, To: to, Schema: schema, At: time.Now().UTC()}); err != nil {
		return err
	}
	if err := a.Repoint(to); err != nil {
		_ = a.ClearPending()
		return err
	}
	return nil
}

// Started is called by serve as it starts, holding the data folder's lock. It counts this start of
// a pending version and returns the pending update when it has already failed MaxFailedStarts times
// and should be rolled back.
func (a App) Started(running string) (*Pending, error) {
	p, err := a.ReadPending()
	if err != nil || p == nil {
		return nil, err
	}
	if running != p.To {
		if running == p.From { // rolled back, or switched back by hand
			return nil, a.ClearPending()
		}
		return nil, nil
	}
	if p.Attempts >= MaxFailedStarts {
		if _, err := os.Stat(a.Exe(p.From)); err == nil {
			return p, nil
		}
	}
	p.Attempts++
	return nil, a.writePending(*p)
}

// RollBack makes the pending update's old version current again and records the new one as bad,
// so it's never offered again. The caller restores the database first.
func (a App) RollBack(p Pending) error {
	if err := a.Repoint(p.From); err != nil {
		return err
	}
	if err := a.addBadVersion(p.To); err != nil {
		return err
	}
	return a.ClearPending()
}

// Uncount takes a clean stop of the pending version off its count: only crashes count.
func (a App) Uncount(running string) error {
	p, err := a.ReadPending()
	if err != nil || p == nil || p.To != running || p.Attempts == 0 {
		return err
	}
	p.Attempts--
	return a.writePending(*p)
}

// Healthy finishes a pending update once its version has run HealthyAfter: the record goes, and
// versions older than the one it replaced are deleted. It returns the pruned versions.
func (a App) Healthy(running string) ([]string, error) {
	p, err := a.ReadPending()
	if err != nil || p == nil || p.To != running {
		return nil, err
	}
	if err := a.ClearPending(); err != nil {
		return nil, err
	}
	return a.Prune(running, p.From), nil
}
