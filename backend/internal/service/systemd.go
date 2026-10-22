package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/yugabyte/meko/backend/internal/paths"
)

// Systemd runs the service as a systemd user unit.
type Systemd struct {
	Home       string
	ConfigHome string // $XDG_CONFIG_HOME, if set
	User       string // for loginctl
	Run        Runner
}

func (d *Systemd) Kind() string       { return "systemd" }
func (d *Systemd) DataDir() string    { return dataDirOf(d.File()) }
func (d *Systemd) StatusHint() string { return "systemctl --user status " + Unit }

func (d *Systemd) File() string {
	base := d.ConfigHome
	if base == "" {
		base = filepath.Join(d.Home, ".config")
	}
	return filepath.Join(base, "systemd", "user", Unit)
}

func (d *Systemd) Installed() bool {
	_, err := os.Stat(d.File())
	return err == nil
}

func (d *Systemd) Preflight() error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemctl wasn't found: install with --no-service and run meko start")
	}
	if _, err := d.Run("systemctl", "--user", "show-environment"); err != nil {
		return fmt.Errorf("there's no systemd user manager for you (%w). Install with --no-service and run meko start", err)
	}
	return nil
}

func (d *Systemd) Install(s Spec) error {
	b, err := render(unitTmpl, s)
	if err != nil {
		return err
	}
	if err = writeFile(d.File(), b); err != nil {
		return err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", Unit}, {"restart", Unit}} {
		if _, err = d.Run("systemctl", append([]string{"--user"}, args...)...); err != nil {
			return err
		}
	}
	return nil
}

func (d *Systemd) Start() error   { return d.ctl("start") }
func (d *Systemd) Stop() error    { return d.ctl("stop") }
func (d *Systemd) Restart() error { return d.ctl("restart") }

func (d *Systemd) ctl(verb string) error {
	_, err := d.Run("systemctl", "--user", verb, Unit)
	return err
}

func (d *Systemd) Running() bool {
	out, _ := d.Run("systemctl", "--user", "is-active", Unit)
	return out == "active"
}

func (d *Systemd) Remove() error {
	if d.Installed() {
		_, _ = d.Run("systemctl", "--user", "disable", "--now", Unit)
	}
	if err := paths.RemoveIfExists(d.File()); err != nil {
		return err
	}
	_, err := d.Run("systemctl", "--user", "daemon-reload")
	return err
}

// Linger makes the user's services start at boot and keep running after logout. It reports what it
// found or did, and a note when it couldn't turn it on.
func (d *Systemd) Linger() (ok bool, detail, note string) {
	if d.User == "" {
		return false, "unknown user", ""
	}
	if out, err := d.Run("loginctl", "show-user", d.User, "-p", "Linger", "--value"); err == nil && out == "yes" {
		return true, "lingering is on for your account", ""
	}
	if _, err := d.Run("loginctl", "enable-linger", d.User); err == nil {
		return true, "lingering turned on for your account", ""
	}
	return false, "lingering is off", "Without it, Meko runs only while you're logged in. Ask an admin to run: " +
		"sudo loginctl enable-linger " + d.User
}
