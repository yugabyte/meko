package service

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRun records the commands it's given and answers from a table of "command line" → output;
// a command in fail returns an error.
type fakeRun struct {
	calls []string
	out   map[string]string
	fail  map[string]bool
}

func (f *fakeRun) run(name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, line)
	if f.fail[line] {
		return "", errors.New(line + ": failed")
	}
	return f.out[line], nil
}

var spec = Spec{
	Exe:        "/home/a b/.meko/app/current/bin/meko",
	DataDir:    `/home/a b/100% $data & "x"`,
	ServiceLog: "/home/a b/.meko/logs/service.log",
	Host:       "devbox",
}

func TestSystemdUnit(t *testing.T) {
	b, err := render(unitTmpl, spec)
	if err != nil {
		t.Fatal(err)
	}
	unit := string(b)
	for _, want := range []string{
		`ExecStart="/home/a b/.meko/app/current/bin/meko" serve --detached --data-dir "/home/a b/100%% $$data & \"x\""`,
		"ConditionHost=devbox", "Restart=always", "RestartPreventExitStatus=75", "TimeoutStopSec=35",
		"KillMode=mixed", "Environment=MEKO_SERVICE=systemd", "StandardError=append:/home/a b/.meko/logs/service.log",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	d := &Systemd{Home: t.TempDir(), Run: (&fakeRun{}).run}
	if err := writeFile(d.File(), b); err != nil {
		t.Fatal(err)
	}
	if got := d.DataDir(); got != spec.DataDir {
		t.Errorf("DataDir %q, want %q", got, spec.DataDir)
	}
}

func TestLaunchdPlist(t *testing.T) {
	b, err := render(plistTmpl, spec)
	if err != nil {
		t.Fatal(err)
	}
	plist := string(b)
	for _, want := range []string{
		"<string>com.yugabyte.meko</string>", "<string>/home/a b/100% $data &amp; &#34;x&#34;/service/meko</string>",
		"<string>/home/a b/100% $data &amp; &#34;x&#34;</string>", "<key>KeepAlive</key>\n  <true/>",
		"<key>ExitTimeOut</key>\n  <integer>35</integer>", "<string>launchd</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "app/current") {
		t.Errorf("the plist runs the versioned binary itself:\n%s", plist)
	}
	d := &Launchd{Home: t.TempDir(), UID: 501, Run: (&fakeRun{}).run}
	if err := writeFile(d.File(), b); err != nil {
		t.Fatal(err)
	}
	if got := d.DataDir(); got != spec.DataDir {
		t.Errorf("DataDir %q, want %q", got, spec.DataDir)
	}
}

func TestSystemdCommands(t *testing.T) {
	f := &fakeRun{out: map[string]string{"systemctl --user is-active meko.service": "active"}}
	d := &Systemd{Home: t.TempDir(), User: "ann", Run: f.run}
	if d.Installed() || d.DataDir() != "" {
		t.Fatal("installed before Install")
	}
	if err := d.Install(spec); err != nil {
		t.Fatal(err)
	}
	if !d.Installed() || !d.Running() {
		t.Fatal("not installed and running after Install")
	}
	if err := d.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove(); err != nil {
		t.Fatal(err)
	}
	if d.Installed() {
		t.Error("the unit file is still there")
	}
	want := []string{
		"systemctl --user daemon-reload", "systemctl --user enable meko.service", "systemctl --user restart meko.service",
		"systemctl --user is-active meko.service", "systemctl --user stop meko.service",
		"systemctl --user disable --now meko.service", "systemctl --user daemon-reload",
	}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if !strings.HasPrefix(d.File(), filepath.Join(d.Home, ".config", "systemd", "user")) {
		t.Errorf("file %s", d.File())
	}
	d.ConfigHome = "/xdg"
	if d.File() != "/xdg/systemd/user/meko.service" {
		t.Errorf("XDG_CONFIG_HOME ignored: %s", d.File())
	}
}

func TestSystemdLinger(t *testing.T) {
	f := &fakeRun{out: map[string]string{"loginctl show-user ann -p Linger --value": "yes"}}
	if ok, _, _ := (&Systemd{User: "ann", Run: f.run}).Linger(); !ok {
		t.Error("lingering on wasn't seen")
	}
	f = &fakeRun{fail: map[string]bool{"loginctl enable-linger ann": true}}
	ok, _, note := (&Systemd{User: "ann", Run: f.run}).Linger()
	if ok || !strings.Contains(note, "sudo loginctl enable-linger ann") {
		t.Errorf("ok %v, note %q", ok, note)
	}
}

func TestLaunchdCommands(t *testing.T) {
	loaded := false
	f := &fakeRun{}
	run := func(name string, args ...string) (string, error) {
		out, err := f.run(name, args...)
		switch strings.Join(args, " ") {
		case "print gui/501/com.yugabyte.meko":
			if !loaded {
				return "", errors.New("not loaded")
			}
			return "gui/501/com.yugabyte.meko = {\n\tstate = running\n\tsubsystem = {\n\t\tstate = idle\n\t}\n}", nil
		case "bootstrap gui/501 " + filepath.Join("HOME", "Library", "LaunchAgents", "com.yugabyte.meko.plist"):
		}
		if args[0] == "bootstrap" {
			loaded = true
		}
		if args[0] == "bootout" {
			loaded = false
		}
		return out, err
	}
	d := &Launchd{Home: t.TempDir(), UID: 501, Run: run, Pause: func(time.Duration) {}}
	spec := spec
	spec.DataDir = t.TempDir()
	if err := d.Install(spec); err != nil {
		t.Fatal(err)
	}
	if !d.Running() {
		t.Fatal("not running after Install")
	}
	if err := d.Restart(); err != nil {
		t.Fatal(err)
	}
	if err := d.Stop(); err != nil {
		t.Fatal(err)
	}
	if d.Running() {
		t.Fatal("running after Stop")
	}
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.File()); !os.IsNotExist(err) {
		t.Error("the plist is still there")
	}
	if _, err := os.Stat(filepath.Dir(Launcher(spec.DataDir))); !os.IsNotExist(err) {
		t.Error("the launcher is still there")
	}
	got := strings.Join(f.calls, "\n")
	for _, want := range []string{
		"launchctl enable gui/501/com.yugabyte.meko",
		"launchctl bootstrap gui/501 " + d.File(),
		"launchctl kickstart -k gui/501/com.yugabyte.meko",
		"launchctl bootout gui/501/com.yugabyte.meko",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("calls lack %q:\n%s", want, got)
		}
	}
}

// The launcher runs the binary with the plist's arguments, whatever the binary's path holds.
func TestLaunchdLauncherRuns(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "it's here")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "meko")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nprintf '%s|' \"$@\"\n"), 0o755); err != nil { //nolint:gosec // G306: a test program
		t.Fatal(err)
	}
	launcher := filepath.Join(t.TempDir(), "service", "meko")
	if _, err := writeIfChanged(launcher, launcherScript(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(launcher, "serve", "--data-dir", "a b").Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "serve|--data-dir|a b|" {
		t.Errorf("the binary got %q", out)
	}
}

// Reinstalling and refreshing leave files whose content didn't change alone, since macOS announces
// a rewritten one as a new background item; a plist from before the launcher is moved onto it.
func TestLaunchdFilesChangeOnlyWithTheirContent(t *testing.T) {
	f := &fakeRun{fail: map[string]bool{"launchctl print gui/501/com.yugabyte.meko": true}} // never loaded
	d := &Launchd{Home: t.TempDir(), UID: 501, Run: f.run}
	spec := spec
	spec.DataDir = t.TempDir()
	if err := d.Install(spec); err != nil {
		t.Fatal(err)
	}
	launcher := Launcher(spec.DataDir)
	stat := func(p string) os.FileInfo {
		t.Helper()
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi
	}
	l0, p0 := stat(launcher), stat(d.File())
	if l0.Mode().Perm() != 0o755 {
		t.Errorf("launcher mode %v", l0.Mode())
	}
	if err := d.Install(spec); err != nil {
		t.Fatal(err)
	}
	if changed, err := d.Refresh(spec); err != nil || changed {
		t.Errorf("Refresh of an unchanged install: changed %v, %v", changed, err)
	}
	if l, p := stat(launcher), stat(d.File()); !os.SameFile(l0, l) || !l.ModTime().Equal(l0.ModTime()) ||
		!os.SameFile(p0, p) || !p.ModTime().Equal(p0.ModTime()) {
		t.Error("an unchanged launcher or plist was written again")
	}

	// A plist written before the launcher ran app/current/bin/meko.
	b, err := os.ReadFile(d.File())
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Replace(string(b), "<string>"+launcher+"</string>", "<string>"+spec.Exe+"</string>", 1)
	if err = os.WriteFile(d.File(), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := d.Refresh(spec); err != nil || !changed {
		t.Fatalf("Refresh of an old plist: changed %v, %v", changed, err)
	}
	if b, _ = os.ReadFile(d.File()); !strings.Contains(string(b), "<string>"+launcher+"</string>") {
		t.Errorf("the plist doesn't run the launcher:\n%s", b)
	}
	if !os.SameFile(l0, stat(launcher)) {
		t.Error("the unchanged launcher was replaced")
	}

	// Another binary (a build from source installed over a release) rewrites the launcher.
	spec.Exe = "/elsewhere/meko"
	if _, err = d.Refresh(spec); err != nil {
		t.Fatal(err)
	}
	if b, _ = os.ReadFile(launcher); !strings.Contains(string(b), "exec '/elsewhere/meko' \"$@\"") {
		t.Errorf("launcher:\n%s", b)
	}
}
