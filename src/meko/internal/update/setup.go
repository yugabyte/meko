package update

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// setupFile is setup-pending in the app folder: written by meko install once its checks pass and
// removed once its server answers, so install.sh, run again over a setup that failed partway,
// finishes it instead of saying the version is up to date. Its first line is the version; a second
// line, "first", says nothing was installed before it. Updates never run meko install, so they
// never leave it behind.
func (a App) setupFile() string { return filepath.Join(a.Root, "setup-pending") }

// MarkSetup records that meko install is setting up version; first is a first install's.
func (a App) MarkSetup(version string, first bool) error {
	if err := os.MkdirAll(a.Root, 0o700); err != nil {
		return err
	}
	s := version + "\n"
	if first {
		s += "first\n"
	}
	return os.WriteFile(a.setupFile(), []byte(s), 0o600)
}

// ClearSetup records that the setup finished.
func (a App) ClearSetup() error {
	if err := os.Remove(a.setupFile()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// SetupPending is the version whose setup hasn't finished, or "".
func (a App) SetupPending() string {
	b, err := os.ReadFile(a.setupFile())
	if err != nil {
		return ""
	}
	v, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(v)
}
