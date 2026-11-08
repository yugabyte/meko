package callhome

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newFirstCommands starts a list in a temp folder, as a first install does.
func newFirstCommands(t *testing.T, started time.Time) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "first-commands.jsonl")
	if err := StartFirstCommands(path, "1.0.0", started); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFirstCommandsFile(t *testing.T) {
	path := newFirstCommands(t, now0)
	// A second start (an install that keeps the data folder) keeps the list.
	status := Command{Cmd: "status", Flags: []string{"verbose", "json"}, TTY: true}
	if err := RecordCommand(path, status, now0); err != nil {
		t.Fatal(err)
	}
	if err := StartFirstCommands(path, "2.0.0", now0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		c := Command{Cmd: "search", Exit: 1, Ms: 12, Version: "1.0.0"}
		if err := RecordCommand(path, c, now0.Add(time.Duration(i)*time.Minute+90*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	list, err := ReadFirstCommands(path, func(c string) bool { return c == "status" || c == "search" })
	if err != nil {
		t.Fatal(err)
	}
	if !list.Started.Equal(now0) || len(list.Commands) != FirstCommandsMax || !list.Full() {
		t.Fatalf("started %s, %d commands", list.Started, len(list.Commands))
	}
	first, second := list.Commands[0], list.Commands[1]
	if first.N != 1 || first.Cmd != "status" || strings.Join(first.Flags, ",") != "json,verbose" || !first.TTY ||
		first.MinsSinceInstall != 0 {
		t.Errorf("first: %+v", first)
	}
	if second.N != 2 || second.Cmd != "search" || second.MinsSinceInstall != 1 || second.Exit != 1 || second.Ms != 12 ||
		second.Flags == nil {
		t.Errorf("second: %+v", second)
	}
	b, _ := os.ReadFile(path) //nolint:gosec // G304: the test's own file
	if n := strings.Count(string(b), "\n"); n != 1+FirstCommandsMax {
		t.Errorf("%d lines, want a header and %d commands", n, FirstCommandsMax)
	}
}

// Without the file (an install from before it, or not a first install), nothing is recorded.
func TestRecordCommandCreatesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "first-commands.jsonl")
	if err := RecordCommand(path, Command{Cmd: "status"}, now0); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("got %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Error("the file was created")
	}
}

// What's read is checked again: a name not in the CLI's list is unknown, a flag that isn't shaped
// like one is dropped, a broken line is skipped, and a 16th from two runs at once is left out.
func TestReadFirstCommandsChecks(t *testing.T) {
	path := newFirstCommands(t, now0)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // G304: the test's own file
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"n":1,"cmd":"my secret query","flags":["datapack","sk-test-token value","x"]}` + "\n" +
		"not json\n")
	for i := range FirstCommandsMax {
		_, _ = f.WriteString(`{"n":` + strconv.Itoa(i+2) + `,"cmd":"status"}` + "\n")
	}
	_ = f.Close()
	list, err := ReadFirstCommands(path, func(c string) bool { return c == "status" })
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Commands) != FirstCommandsMax {
		t.Fatalf("%d commands", len(list.Commands))
	}
	if c := list.Commands[0]; c.Cmd != UnknownCommand || strings.Join(c.Flags, ",") != "datapack,x" {
		t.Errorf("not checked: %+v", c)
	}
	if c := list.Commands[FirstCommandsMax-1]; c.N != FirstCommandsMax || c.Cmd != "status" {
		t.Errorf("last: %+v", c)
	}
}

// The heartbeat carries the first commands until a report with all of them is taken.
func TestHeartbeatCarriesFirstCommands(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	path := newFirstCommands(t, now0)
	s := New(Options{Store: r.st, Config: r.cfg, FirstCommands: path, Commands: []string{"status"},
		Now: func() time.Time { return r.clock }, Rand: func() float64 { return 0.5 }})
	for i := range 3 {
		if err := RecordCommand(path, Command{Cmd: []string{"status", "nosuch", "status"}[i], TTY: i == 0},
			now0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if !s.Tick(ctx) {
		t.Fatal("not sent")
	}
	got := r.coll.last(t).Payload.FirstCommands
	if len(got) != 3 || got[0].Cmd != "status" || !got[0].TTY || got[1].Cmd != UnknownCommand || got[2].N != 3 ||
		got[2].MinsSinceInstall != 2 {
		t.Fatalf("first commands: %+v", got)
	}
	// A partial list keeps being sent.
	r.clock = r.clock.Add(CheckEvery)
	for range FirstCommandsMax {
		_ = RecordCommand(path, Command{Cmd: "status"}, r.clock)
	}
	if !s.Tick(ctx) || len(r.coll.last(t).Payload.FirstCommands) != FirstCommandsMax {
		t.Fatal("the full list wasn't sent")
	}
	r.clock = r.clock.Add(CheckEvery)
	if !s.Tick(ctx) {
		t.Fatal("not sent")
	}
	if got := r.coll.last(t).Payload.FirstCommands; got != nil {
		t.Errorf("sent again after the full list was taken: %d", len(got))
	}
}

// A full list the service refused is sent again.
func TestFirstCommandsResentAfterAFailure(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	path := newFirstCommands(t, now0)
	for range FirstCommandsMax {
		_ = RecordCommand(path, Command{Cmd: "status"}, now0)
	}
	s := New(Options{Store: r.st, Config: r.cfg, FirstCommands: path, Commands: []string{"status"},
		Now: func() time.Time { return r.clock }, Rand: func() float64 { return 0.5 }})
	r.coll.status = http.StatusBadRequest
	if s.Tick(ctx) {
		t.Fatal("sent")
	}
	r.coll.status = 0
	if !s.Tick(ctx) || len(r.coll.last(t).Payload.FirstCommands) != FirstCommandsMax {
		t.Error("the list wasn't sent again")
	}
}

func TestSendUninstall(t *testing.T) {
	coll := &collector{}
	srv := httptest.NewServer(coll)
	t.Cleanup(srv.Close)
	u := Uninstall{Purge: true, Version: "1.0.0", Platform: "linux", Arch: "amd64", Service: "systemd",
		FirstDay: "2026-11-09"}
	if err := SendUninstall(context.Background(), srv.Client(), srv.URL, UninstallCommand, "id-1", u, now0); err != nil {
		t.Fatal(err)
	}
	var e UninstallEnvelope
	if err := json.Unmarshal(coll.bodies[0], &e); err != nil {
		t.Fatal(err)
	}
	if e.CallhomeType != "meko" || e.Command != "uninstall" || e.InstallID != "id-1" || e.Version != "1.0.0" ||
		e.Timestamp != "2026-11-09T12:00:00.000000" || coll.agents[0] != "meko/1.0.0" {
		t.Errorf("envelope: %+v", e)
	}
	if p := e.Payload; p.Schema != 1 || !p.Purge || p.FirstDay != "2026-11-09" || p.FirstCommands == nil {
		t.Errorf("payload: %+v", p)
	}
	if !strings.Contains(string(coll.bodies[0]), `"firstCommands":[]`) {
		t.Errorf("no list is an empty one: %s", coll.bodies[0])
	}
	// A service that doesn't answer is given up on.
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(hang.Close)
	t.Cleanup(func() { close(release) }) // runs first: Close waits for the handler
	begun := time.Now()
	if err := SendUninstall(context.Background(), hang.Client(), hang.URL, DestroyCommand, "id-1", u, now0); err == nil {
		t.Error("no error from a service that didn't answer")
	}
	if took := time.Since(begun); took > FailureTimeout+2*time.Second {
		t.Errorf("waited %s", took)
	}
}
