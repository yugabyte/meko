package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yugabyte/meko/backend/internal/paths"
)

// Launchd runs the service as a LaunchAgent in the user's GUI session.
type Launchd struct {
	Home string
	UID  int
	Run  Runner
	// Pause waits between checks; tests make it instant.
	Pause func(time.Duration)
}

func (d *Launchd) Kind() string { return "launchd" }
func (d *Launchd) File() string {
	return filepath.Join(d.Home, "Library", "LaunchAgents", Label+".plist")
}
func (d *Launchd) domain() string     { return fmt.Sprintf("gui/%d", d.UID) }
func (d *Launchd) target() string     { return d.domain() + "/" + Label }
func (d *Launchd) DataDir() string    { return dataDirOf(d.File()) }
func (d *Launchd) StatusHint() string { return "launchctl print " + d.target() }

func (d *Launchd) Installed() bool {
	_, err := os.Stat(d.File())
	return err == nil
}

func (d *Launchd) loaded() bool {
	_, err := d.Run("launchctl", "print", d.target())
	return err == nil
}

func (d *Launchd) pause(t time.Duration) {
	if d.Pause != nil {
		d.Pause(t)
		return
	}
	time.Sleep(t)
}

// Preflight needs a GUI session: LaunchAgents run in one, and there's none over SSH alone.
func (d *Launchd) Preflight() error {
	if _, err := d.Run("launchctl", "print", d.domain()); err != nil {
		return fmt.Errorf("there's no login session for launchd's %s domain (log in at the Mac itself, not only "+
			"over SSH), or install with --no-service and run meko start: %w", d.domain(), err)
	}
	return nil
}

func (d *Launchd) Install(s Spec) error {
	b, err := render(plistTmpl, s)
	if err != nil {
		return err
	}
	if err = d.bootout(); err != nil {
		return err
	}
	if err = writeFile(d.File(), b); err != nil {
		return err
	}
	// A job turned off with launchctl disable stays off through bootstrap.
	_, _ = d.Run("launchctl", "enable", d.target())
	_, err = d.Run("launchctl", "bootstrap", d.domain(), d.File())
	return err
}

func (d *Launchd) Start() error {
	if d.loaded() {
		_, err := d.Run("launchctl", "kickstart", d.target()) // without -k it leaves a running job alone
		return err
	}
	_, err := d.Run("launchctl", "bootstrap", d.domain(), d.File())
	return err
}

// Stop unloads the job: with KeepAlive, a job that's only killed starts again.
func (d *Launchd) Stop() error { return d.bootout() }

func (d *Launchd) bootout() error {
	if !d.loaded() {
		return nil
	}
	if _, err := d.Run("launchctl", "bootout", d.target()); err != nil && d.loaded() {
		return err
	}
	for i := 0; i < 400 && d.loaded(); i++ { // bootout returns before the job has gone
		d.pause(100 * time.Millisecond)
	}
	if d.loaded() {
		return fmt.Errorf("%s didn't stop within 40 s. Check: %s", Label, d.StatusHint())
	}
	return nil
}

func (d *Launchd) Restart() error {
	if !d.loaded() {
		return d.Start()
	}
	_, err := d.Run("launchctl", "kickstart", "-k", d.target())
	return err
}

func (d *Launchd) Running() bool {
	out, err := d.Run("launchctl", "print", d.target())
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), " = "); ok && k == "state" {
			return v == "running" // the job's own line comes first; nested blocks come later
		}
	}
	return false
}

func (d *Launchd) Remove() error {
	if err := d.bootout(); err != nil {
		return err
	}
	return paths.RemoveIfExists(d.File())
}
