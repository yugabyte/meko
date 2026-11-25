package service

import (
	"os"
	"strings"
	"testing"
	"time"
)

var menuBarSpec = MenuBarSpec{
	App:     "/home/a b/.local/share/meko/app/current/bin/Meko.app",
	Meko:    "/home/a b/.local/share/meko/app/current/bin/meko",
	DataDir: "/home/a b/.meko & co",
	Log:     "/home/a b/.meko/logs/menubar.log",
}

func TestMenuBarPlist(t *testing.T) {
	b, err := renderMenuBar(menuBarSpec)
	if err != nil {
		t.Fatal(err)
	}
	plist := string(b)
	for _, want := range []string{
		"<string>com.yugabyte.meko.menubar</string>",
		"<string>/home/a b/.local/share/meko/app/current/bin/Meko.app/Contents/MacOS/meko-menubar</string>",
		"<string>--meko</string>\n    <string>/home/a b/.local/share/meko/app/current/bin/meko</string>",
		"<string>--data-dir</string>\n    <string>/home/a b/.meko &amp; co</string>",
		"<key>SuccessfulExit</key>\n    <false/>", "<string>Aqua</string>",
		"<string>/home/a b/.meko/logs/menubar.log</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
	if got := MenuBarAppFor(menuBarSpec.Meko); got != menuBarSpec.App {
		t.Errorf("MenuBarAppFor %q", got)
	}
}

// Start writes the job and bootstraps it; a second start leaves the plist alone (no new login item
// notification) and kickstarts the loaded job, which starts the app again after Quit. Remove unloads
// the job and deletes the plist.
func TestMenuBarStartStop(t *testing.T) {
	f := &fakeRun{out: map[string]string{}, fail: map[string]bool{}}
	d := NewMenuBar(t.TempDir(), f.run)
	d.UID = 501
	d.Pause = func(_ time.Duration) {}
	target := "gui/501/" + MenuBarLabel
	f.fail["launchctl print "+target] = true // not loaded yet
	if err := d.StartMenuBar(menuBarSpec); err != nil {
		t.Fatal(err)
	}
	if !d.Installed() || !strings.HasSuffix(d.File(), "/Library/LaunchAgents/com.yugabyte.meko.menubar.plist") {
		t.Fatalf("plist %s not written", d.File())
	}
	if got := strings.Join(f.calls, "\n"); !strings.Contains(got, "launchctl enable "+target) ||
		!strings.Contains(got, "launchctl bootstrap gui/501 "+d.File()) || strings.Contains(got, "kickstart") {
		t.Errorf("first start ran:\n%s", got)
	}
	st, _ := os.Stat(d.File())

	f.calls = nil
	delete(f.fail, "launchctl print "+target) // loaded now
	if err := d.StartMenuBar(menuBarSpec); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.calls, "\n"); strings.Contains(got, "bootout") || strings.Contains(got, "bootstrap") ||
		!strings.Contains(got, "launchctl kickstart "+target) {
		t.Errorf("second start ran:\n%s", got)
	}
	if st2, _ := os.Stat(d.File()); !st2.ModTime().Equal(st.ModTime()) {
		t.Error("an unchanged plist was written again")
	}

	// A changed spec (another data folder) unloads the old definition before writing the new one.
	f.calls = nil
	moved := menuBarSpec
	moved.DataDir = "/elsewhere"
	unloaded := false
	d.Run = func(name string, args ...string) (string, error) {
		line := name + " " + strings.Join(args, " ")
		if line == "launchctl bootout "+target {
			unloaded = true
		}
		if line == "launchctl print "+target && unloaded {
			f.calls = append(f.calls, line)
			return "", os.ErrNotExist
		}
		return f.run(name, args...)
	}
	if err := d.StartMenuBar(moved); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(d.File()); !unloaded || !strings.Contains(string(b), "/elsewhere") ||
		!strings.Contains(strings.Join(f.calls, "\n"), "launchctl bootstrap") {
		t.Errorf("a changed job: unloaded %v, ran:\n%s", unloaded, strings.Join(f.calls, "\n"))
	}

	unloaded = false
	if err := d.Remove(); err != nil {
		t.Fatal(err)
	}
	if d.Installed() || !unloaded {
		t.Errorf("remove: installed %v, unloaded %v", d.Installed(), unloaded)
	}
}
