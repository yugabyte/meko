package service

import (
	"bytes"
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

// Launcher is the program the LaunchAgent runs for exe. A release's exe is <app>/current/bin/meko,
// and macOS's Background Task Management announces "Background Items Added" again whenever the
// program a LaunchAgent names is another file, as that one is after every update, so the plist
// names <app>/service/meko instead: a script that execs exe, which an update never replaces. It's
// in the app folder, not the data folder, so the service meko destroy keeps still starts. A build
// from source doesn't update itself, and the plist names exe.
func Launcher(exe string) string {
	bin := filepath.Dir(exe)
	if filepath.Base(bin) != "bin" || filepath.Base(filepath.Dir(bin)) != "current" {
		return exe
	}
	return filepath.Join(filepath.Dir(filepath.Dir(bin)), "service", "meko")
}

// launcherHead starts every launcher, so Remove deletes only one of ours.
const launcherHead = "#!/bin/sh\n# Meko's LaunchAgent runs this file"

// launcherScript is the launcher's text: it runs exe with the plist's arguments.
func launcherScript(exe string) []byte {
	return []byte(launcherHead + ", written by meko install. Updates leave it alone, so\n" +
		"# macOS doesn't announce Meko as a new background item after each one.\n" +
		"exec '" + strings.ReplaceAll(exe, "'", `'\''`) + "' \"$@\"\n")
}

// writeFiles writes the launcher and the plist, each only when its content changed: Background Task
// Management takes a rewritten file for a new item too. It reports whether the plist changed.
func (d *Launchd) writeFiles(s Spec) (bool, error) {
	b, err := render(plistTmpl, s)
	if err != nil {
		return false, err
	}
	if l := Launcher(s.Exe); l != s.Exe {
		// The app folder is the user's own (0700), and launchd runs the job as the user.
		if _, err = writeIfChanged(l, launcherScript(s.Exe), 0o700, 0o700); err != nil {
			return false, err
		}
	}
	return writeIfChanged(d.File(), b, 0o600, 0o755)
}

func (d *Launchd) Install(s Spec) error {
	if err := d.bootout(); err != nil {
		return err
	}
	if _, err := d.writeFiles(s); err != nil {
		return err
	}
	// A job turned off with launchctl disable stays off through bootstrap.
	_, _ = d.Run("launchctl", "enable", d.target())
	_, err := d.Run("launchctl", "bootstrap", d.domain(), d.File())
	return err
}

// Refresh brings the launcher and the plist up to date with s without reloading the job, and
// reports whether the plist changed. launchd keeps running the job it loaded until the next login
// or Install; Background Task Management reads the files. serve runs it at every start, so an
// install whose plist ran app/current/bin/meko moves to the launcher with its next update.
func (d *Launchd) Refresh(s Spec) (bool, error) {
	return d.writeFiles(s)
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
	// The launcher the plist runs, unless it runs a build from source itself.
	if l := programOf(d.File()); l != "" {
		if b, err := os.ReadFile(l); err == nil && bytes.HasPrefix(b, []byte(launcherHead)) { //nolint:gosec // G304: the plist's program
			if err = paths.RemoveIfExists(l); err != nil {
				return err
			}
			_ = os.Remove(filepath.Dir(l)) // only when empty
		}
	}
	return paths.RemoveIfExists(d.File())
}

// writeIfChanged writes data to path with mode unless the file already holds exactly that, and
// reports whether it wrote, creating its folder with dirMode. The new file is renamed into place,
// so it's never seen half written.
func writeIfChanged(path string, data []byte, mode, dirMode os.FileMode) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) { //nolint:gosec // G304: our own file
		if fi, serr := os.Stat(path); serr == nil && fi.Mode().Perm() == mode {
			return false, nil
		}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil { //nolint:gosec // G301: 0755 only for ~/Library/LaunchAgents
		return false, err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // gone after the rename
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	return err == nil, err
}
