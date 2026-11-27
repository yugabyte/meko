package service

import (
	"bytes"
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// The menu bar app (macOS): Meko.app, beside the meko binary (bin/Meko.app in a release, and after
// make menubar), started at login by a LaunchAgent of its own.
const (
	MenuBarLabel   = "com.yugabyte.meko.menubar" // its LaunchAgent
	MenuBarApp     = "Meko.app"                  // the bundle, in the meko binary's folder
	MenuBarProgram = "meko-menubar"              // its executable, which Login Items names it by
)

// ErrNoMenuBar means the OS has no menu bar app: it's macOS's alone.
var ErrNoMenuBar = errors.New("the menu bar app runs only on macOS")

// MenuBarSpec is what the menu bar app's job runs.
type MenuBarSpec struct {
	App     string // the bundle: <folder of meko>/Meko.app
	Meko    string // the meko binary it asks for the server's address and token (meko token --json)
	DataDir string // passed on as --data-dir, so it reaches the Meko the service runs
	Log     string // its stderr
}

// MenuBarAppFor is the bundle that goes with the meko binary exe.
func MenuBarAppFor(exe string) string { return filepath.Join(filepath.Dir(exe), MenuBarApp) }

// MenuBarLauncher is the program the app's LaunchAgent runs, as Launcher is Meko's: for a release's
// meko (<app>/current/bin/meko), <app>/service/meko-menubar, a script that execs the app's program in
// app/current. Background Task Management announces "Background Items Added" again whenever the
// program a plist names is another file, as app/current/... is after every update; the script never
// changes. A build from source doesn't update itself, and the plist names the app's program.
func MenuBarLauncher(s MenuBarSpec) string {
	if l := Launcher(s.Meko); l != s.Meko {
		return filepath.Join(filepath.Dir(l), MenuBarProgram)
	}
	return s.program()
}

func (s MenuBarSpec) program() string {
	return filepath.Join(s.App, "Contents", "MacOS", MenuBarProgram)
}

// menuBarScript is the launcher's text. It starts with launcherHead, so Remove deletes it.
func menuBarScript(program string) []byte {
	return []byte(launcherHead + ", written by meko menubar start. Updates leave it alone,\n" +
		"# so macOS doesn't announce the menu bar app as a new background item after each one.\n" +
		"exec '" + strings.ReplaceAll(program, "'", `'\''`) + "' \"$@\"\n")
}

// NewMenuBar is the menu bar app's LaunchAgent for the user whose home is home.
func NewMenuBar(home string, run Runner) *Launchd {
	return &Launchd{Home: home, UID: os.Getuid(), Run: run, Job: MenuBarLabel}
}

//go:embed menubar.plist.tmpl
var menuBarTmpl string

func renderMenuBar(s MenuBarSpec) ([]byte, error) {
	t, err := template.New("menubar").Funcs(tmplFuncs).Option("missingkey=error").Parse(menuBarTmpl)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	err = t.Execute(&b, struct {
		MenuBarSpec
		Label, Program string
	}{s, MenuBarLabel, MenuBarLauncher(s)})
	return b.Bytes(), err
}

// StartMenuBar writes the app's launcher and job, loads it and starts the app; one already running is
// left alone. Each file is written only when it changes: macOS's Background Task Management counts a
// rewritten one as a new login item and says so in a notification.
func (d *Launchd) StartMenuBar(s MenuBarSpec) error {
	b, err := renderMenuBar(s)
	if err != nil {
		return err
	}
	if l := MenuBarLauncher(s); l != s.program() {
		// The app folder is the user's own (0700), and launchd runs the job as the user.
		if _, err = writeIfChanged(l, menuBarScript(s.program()), 0o700, 0o700); err != nil {
			return err
		}
	}
	if old, rerr := os.ReadFile(d.File()); rerr != nil || !bytes.Equal(old, b) {
		if err = d.bootout(); err != nil { // launchd keeps a loaded job's old definition
			return err
		}
		if _, err = writeIfChanged(d.File(), b, 0o600, 0o755); err != nil {
			return err
		}
	}
	_, _ = d.Run("launchctl", "enable", d.target())
	if !d.loaded() {
		_, err = d.Run("launchctl", "bootstrap", d.domain(), d.File())
		return err
	}
	_, err = d.Run("launchctl", "kickstart", d.target()) // starts it again after Quit
	return err
}
