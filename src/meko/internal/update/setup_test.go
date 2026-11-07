package update

import (
	"os"
	"path/filepath"
	"testing"
)

// The setup marker names the version being set up, says whether it's a first install (install.sh
// reads the file too), and goes once the setup finishes.
func TestSetupMarker(t *testing.T) {
	a := App{Root: filepath.Join(t.TempDir(), "app")}
	if v := a.SetupPending(); v != "" {
		t.Errorf("no marker: %q", v)
	}
	if err := a.MarkSetup("1.2.3", true); err != nil {
		t.Fatal(err)
	}
	if v := a.SetupPending(); v != "1.2.3" {
		t.Errorf("marked: %q", v)
	}
	if b, _ := os.ReadFile(a.setupFile()); string(b) != "1.2.3\nfirst\n" {
		t.Errorf("file: %q", b)
	}
	if err := a.ClearSetup(); err != nil {
		t.Fatal(err)
	}
	if err := a.ClearSetup(); err != nil {
		t.Errorf("clearing twice: %v", err)
	}
	if v := a.SetupPending(); v != "" {
		t.Errorf("cleared: %q", v)
	}
}
