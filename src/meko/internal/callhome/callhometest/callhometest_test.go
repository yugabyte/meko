package callhometest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/callhome"
)

func TestMain(m *testing.M) { os.Exit(Run(m)) }

// forget drops what a test turned away on purpose, so the run doesn't fail for it.
func forget(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		mu.Lock()
		blocked = nil
		mu.Unlock()
		if err := os.Truncate(file, 0); err != nil {
			t.Error(err)
		}
	})
}

// A failed install's report to DefaultURL is turned away, never sent, and counted, with its
// command and version; a test server still gets its requests.
func TestGuardTurnsAwayTheDefaultURL(t *testing.T) {
	forget(t)
	if os.Getenv(EnvGuard) == "" || !parent {
		t.Fatalf("the guard's file isn't set for children: %q", os.Getenv(EnvGuard))
	}
	before := len(Blocked())
	err := callhome.SendFailure(context.Background(), http.DefaultClient, callhome.DefaultURL, "test-install",
		callhome.Failure{Step: callhome.StepData, Kind: callhome.KindError, Version: "0.2.0"}, time.Now())
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("sent to DefaultURL: %v", err)
	}
	got := Blocked()
	if len(got) != before+1 || !strings.Contains(got[before], `command "install_failure", version "0.2.0"`) {
		t.Errorf("blocked: %q", got)
	}

	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()
	if err := callhome.SendFailure(context.Background(), http.DefaultClient, srv.URL, "test-install",
		callhome.Failure{Step: callhome.StepData, Kind: callhome.KindError}, time.Now()); err != nil || !hit {
		t.Errorf("a test server didn't get its report: %v", err)
	}
	if len(Blocked()) != before+1 {
		t.Errorf("a test server's report was counted: %q", Blocked())
	}
}

// What a child process turned away, in the guard's file, counts too.
func TestBlockedReadsTheChildren(t *testing.T) {
	forget(t)
	line := "POST https://example.invalid: command \"uninstall\", version \"dev\" (from pid 1)"
	if err := os.WriteFile(file, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Blocked(); len(got) != 1 || got[0] != line {
		t.Errorf("blocked: %q", got)
	}
}

// fakeT records what Check's cleanup says.
type fakeT struct {
	testing.TB
	cleanups []func()
	errs     []string
}

func (f *fakeT) Helper()                        {}
func (f *fakeT) Cleanup(fn func())              { f.cleanups = append(f.cleanups, fn) }
func (f *fakeT) Errorf(format string, a ...any) { f.errs = append(f.errs, fmt.Sprintf(format, a...)) }

// Check fails a test that sent a report to DefaultURL while it ran, and only that one.
func TestCheckFailsTheTestThatSent(t *testing.T) {
	forget(t)
	quiet := &fakeT{}
	Check(quiet)
	quiet.cleanups[0]()
	if len(quiet.errs) != 0 {
		t.Errorf("a test that sent nothing failed: %q", quiet.errs)
	}

	sender := &fakeT{}
	Check(sender)
	sendToDefault(t, "heartbeat")
	sender.cleanups[0]()
	if len(sender.errs) != 1 || !strings.Contains(sender.errs[0], `command "heartbeat", version "dev"`) {
		t.Errorf("the test that sent: %q", sender.errs)
	}
}

// sendToDefault posts a report with command cmd to DefaultURL, which the guard turns away.
func sendToDefault(t *testing.T, cmd string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, callhome.DefaultURL,
		strings.NewReader(`{"command":"`+cmd+`","version":"dev"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res, err := http.DefaultClient.Do(req); err == nil {
		_ = res.Body.Close()
		t.Fatal("sent to DefaultURL")
	}
}

// Check names the report the test sent, not a child's from an earlier test still in the file.
func TestCheckNamesThisTestsReport(t *testing.T) {
	forget(t)
	if err := os.WriteFile(file, []byte("POST https://example.invalid: command \"earlier\" (from pid 1)\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	ft := &fakeT{}
	Check(ft)
	sendToDefault(t, "heartbeat")
	ft.cleanups[0]()
	if len(ft.errs) != 1 || !strings.Contains(ft.errs[0], `command "heartbeat"`) ||
		strings.Contains(ft.errs[0], "earlier") {
		t.Errorf("the test that sent: %q", ft.errs)
	}
}

// Without the file (it couldn't be made), the test binary still counts what it turned away.
func TestGuardCountsWithoutTheFile(t *testing.T) {
	forget(t)
	saved := file
	file = ""
	defer func() { file = saved }()
	sendToDefault(t, "uninstall")
	if got := Blocked(); len(got) != 1 || !strings.Contains(got[0], `command "uninstall"`) {
		t.Errorf("blocked: %q", got)
	}
}
