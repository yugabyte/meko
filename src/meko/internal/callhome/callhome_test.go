package callhome

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/store"
)

var now0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

type collector struct {
	mu     sync.Mutex
	status int
	bodies [][]byte
	agents []string
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, b)
	c.agents = append(c.agents, r.Header.Get("User-Agent"))
	if c.status != 0 {
		w.WriteHeader(c.status)
		return
	}
	_, _ = w.Write([]byte(`"Success"`))
}

func (c *collector) last(t *testing.T) Envelope {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var e Envelope
	if err := json.Unmarshal(c.bodies[len(c.bodies)-1], &e); err != nil {
		t.Fatal(err)
	}
	return e
}

type rig struct {
	st    *store.Store
	coll  *collector
	clock time.Time
	logs  *bytes.Buffer
	cfg   config.CallHome
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "meko.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureDefaultDatapack(ctx); err != nil {
		t.Fatal(err)
	}
	coll := &collector{}
	srv := httptest.NewServer(coll)
	t.Cleanup(srv.Close)
	return &rig{st: st, coll: coll, clock: now0, logs: &bytes.Buffer{},
		cfg: config.CallHome{Enabled: true, URL: srv.URL + "/"}}
}

func (r *rig) sender(release bool) *Sender {
	return New(Options{Store: r.st, Config: r.cfg, Release: release, Tools: []string{"context_search", "memory_add"},
		Log: slog.New(slog.NewTextHandler(r.logs, nil)), Now: func() time.Time { return r.clock },
		Rand: func() float64 { return 0.5 },
		Facts: func(context.Context) Install {
			return Install{Version: "0.20261109.1", Commit: "abc1234", Platform: "linux", Arch: "amd64", Go: "go1.26.0",
				Service: "systemd", Auth: "token", Secrets: "file", Update: Update{Channel: "stable", Auto: true},
				ContextEngine: "ready", Extensions: map[string]bool{"kode": true}}
		}})
}

func TestClient(t *testing.T) {
	for in, want := range map[string]string{
		"claude_code:secret-repo": "claude_code", "Claude_Code": "claude_code", "agent-k-local": "kode",
		"agentk": "kode", "kode": "kode", "meko_cli": "meko_cli", "meko_web_ui": "meko_web_ui",
		"meko_agent": "none", "": "none",
		"kiro:x": "kiro", "my-own-bot": "other", "cursor:repo": "cursor",
	} {
		if got := Client(in); got != want {
			t.Errorf("Client(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTimestamp(t *testing.T) {
	ts := Timestamp(time.Date(2026, 11, 9, 17, 5, 32, 123456789, time.FixedZone("x", 3600)))
	if ts != "2026-11-09T16:05:32.123456" {
		t.Errorf("got %s", ts)
	}
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{6}$`).MatchString(Timestamp(now0)) {
		t.Errorf("got %s", Timestamp(now0))
	}
}

func TestSendsCountsOnly(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	dp, err := r.st.CreateDatapack(ctx, "hush-datapack", "a hush description")
	if err != nil {
		t.Fatal(err)
	}
	// Context Gaps: two poor searches, a person's dismissal and Kode's learn step's teach (an agent's
	// mark names its conversation).
	for _, key := range []string{"hush one", "hush two"} {
		if err = r.st.AddAsk(ctx, store.Ask{DatapackID: dp.ID, Key: key, At: now0.Add(-time.Hour), Query: key,
			Quality: "empty", Tool: "context_search"}); err != nil {
			t.Fatal(err)
		}
	}
	if err = r.st.MarkQuestion(ctx, dp.ID, "hush one", store.QuestionMark{State: "dismissed",
		At: now0.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err = r.st.MarkQuestion(ctx, dp.ID, "hush two", store.QuestionMark{State: "taught", At: now0.Add(-time.Minute),
		By: "hush-conversation", Text: "hush answer"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []store.Call{
		{At: now0.Add(-time.Hour), Tool: "context_search", AgentID: "claude_code:hush-repo", OK: true, Ms: 10,
			TokensSent: 100, TokensSaved: 900},
		{At: now0.Add(-time.Hour), Tool: "context_search", AgentID: "claude_code:hush-repo", ErrorCode: "x"},
		{At: now0.Add(-2 * time.Hour), Tool: "memory_add", AgentID: "agent-k-local", OK: true},
		{At: now0.Add(-3 * time.Hour), Tool: "hush_tool", AgentID: "hush-bot", OK: true},
		{At: now0.Add(-24 * time.Hour), Tool: "memory_add", AgentID: "meko_cli", OK: true},
	} {
		if err := r.st.AddCall(ctx, c, nil); err != nil {
			t.Fatal(err)
		}
	}
	s := r.sender(false)
	if !s.Tick(ctx) {
		t.Fatalf("nothing sent: %s", r.logs)
	}
	raw := string(r.coll.bodies[0])
	for _, secret := range []string{"hush", "description"} {
		if strings.Contains(raw, secret) {
			t.Errorf("the body has %q: %s", secret, raw)
		}
	}
	if r.coll.agents[0] != "meko/0.20261109.1" {
		t.Errorf("User-Agent %q", r.coll.agents[0])
	}
	e := r.coll.last(t)
	id, _ := r.st.EnsureInstallID(ctx)
	if e.CallhomeType != "meko" || e.Command != "heartbeat" || e.Timestamp != "2026-11-09T12:00:00.000000" ||
		e.InstallID != id || e.Version != "0.20261109.1" {
		t.Errorf("envelope: %+v", e)
	}
	p := e.Payload
	if p.Schema != 4 || p.Install.FirstDay != "2026-11-09" || p.Install.Commit != "abc1234" ||
		!p.Install.Extensions["kode"] {
		t.Errorf("install: %+v", p.Install)
	}
	if p.Totals.Datapacks != 2 || p.Totals.DBBytes <= 0 {
		t.Errorf("totals: %+v", p.Totals)
	}
	if len(p.Days) != FirstReportDays || p.Days[len(p.Days)-1].Day != "2026-11-09" {
		t.Fatalf("days: %d, last %+v", len(p.Days), p.Days[len(p.Days)-1])
	}
	today, yesterday := p.Days[len(p.Days)-1], p.Days[len(p.Days)-2]
	if today.Calls != 4 || today.Errors != 1 || today.Ms != 10 || today.TokensSent != 100 || today.TokensSaved != 900 {
		t.Errorf("today: %+v", today)
	}
	wantTools := map[string]int{"context_search": 2, "memory_add": 1, "other": 1}
	wantClients := map[string]int{"claude_code": 2, "kode": 1, "other": 1}
	for k, v := range wantTools {
		if today.ByTool[k] != v {
			t.Errorf("byTool: %v, want %v", today.ByTool, wantTools)
		}
	}
	for k, v := range wantClients {
		if today.ByClient[k] != v {
			t.Errorf("byClient: %v, want %v", today.ByClient, wantClients)
		}
	}
	if want := (ContextGaps{Empty: 2, Dismissed: 1, AgentTaught: 1}); today.ContextGaps != want {
		t.Errorf("contextGaps: %+v, want %+v", today.ContextGaps, want)
	}
	const gaps = `"contextGaps":{"empty":2,"weak":0,"dismissed":1,"taught":0,"agentDismissed":0,"agentTaught":1}`
	if !strings.Contains(raw, gaps) {
		t.Errorf("contextGaps' names in the body: %s", raw)
	}
	if yesterday.Calls != 1 || yesterday.ByClient["meko_cli"] != 1 {
		t.Errorf("yesterday: %+v", yesterday)
	}
	if p.Days[0].ByTool == nil || p.Days[0].ByClient == nil {
		t.Errorf("an empty day has nil maps: %+v", p.Days[0])
	}
}

func TestSchedule(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	s := r.sender(false)
	if !s.Tick(ctx) {
		t.Fatal("the first report wasn't sent")
	}
	// The first day: a report at every hourly check, not between them.
	r.clock = now0.Add(FirstDaySendAfter - time.Minute)
	if s.Tick(ctx) {
		t.Error("sent before the next hourly report was due")
	}
	r.clock = now0.Add(CheckEvery)
	if !s.Tick(ctx) {
		t.Fatal("the second report wasn't sent")
	}
	if got := r.coll.last(t); len(got.Payload.Days) != ReportDays || got.Payload.Install.FirstDay != "2026-11-09" {
		t.Errorf("the second report: %d days, first day %s", len(got.Payload.Days), got.Payload.Install.FirstDay)
	}
	for r.clock.Add(CheckEvery).Before(now0.Add(FirstDay)) {
		r.clock = r.clock.Add(CheckEvery)
		if !s.Tick(ctx) {
			t.Fatalf("not sent at %s", r.clock)
		}
	}
	if n := len(r.coll.bodies); n != 24 {
		t.Errorf("%d reports in the first day, want 24", n)
	}
	// Then once a day, across restarts too.
	lastHourly := r.clock
	r.clock = r.clock.Add(CheckEvery)
	if r.sender(false).Tick(ctx) {
		t.Error("sent hourly after the first day")
	}
	r.clock = lastHourly.Add(SendAfter)
	if !r.sender(false).Tick(ctx) {
		t.Error("the daily report wasn't sent")
	}
}

func TestSwitches(t *testing.T) {
	ctx := context.Background()

	// A build from source sends only to a configured URL.
	r := newRig(t)
	r.cfg.URL = ""
	dev := r.sender(false)
	if dev.Endpoint() != "" || dev.Tick(ctx) {
		t.Error("a dev build sent")
	}
	if st, _ := dev.State(ctx); !st.Enabled || st.Sends {
		t.Errorf("dev state: %+v", st)
	}
	if got := r.sender(true).Endpoint(); got != "https://diagnostics.meko.yugabyte.com" {
		t.Errorf("a release sends to %q", got)
	}

	// The web UI's setting, read at every check.
	r = newRig(t)
	s := r.sender(false)
	if st, err := s.SetEnabled(ctx, false); err != nil || st != (State{By: OffByWebUI}) {
		t.Fatalf("turned off: %+v %v", st, err)
	}
	if s.Tick(ctx) || len(r.coll.bodies) != 0 {
		t.Error("sent while turned off")
	}
	if st, err := s.SetEnabled(ctx, true); err != nil || st != (State{Enabled: true, Sends: true, URL: r.cfg.URL}) {
		t.Fatalf("turned on: %+v %v", st, err)
	}
	if !s.Tick(ctx) {
		t.Error("not sent once turned on again")
	}

	// Config or the environment turned it off: the web UI can't turn it on.
	r = newRig(t)
	r.cfg.Enabled, r.cfg.OffBy = false, config.OffByEnv
	s = r.sender(true)
	if st, _ := s.SetEnabled(ctx, true); st != (State{ForcedOff: true, By: config.OffByEnv}) {
		t.Errorf("forced off: %+v", st)
	}
	if s.Tick(ctx) || len(r.coll.bodies) != 0 {
		t.Error("sent while forced off")
	}
}

func TestRetriesAfterAFailure(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.coll.status = http.StatusBadRequest
	s := r.sender(false)
	if s.Tick(ctx) {
		t.Fatal("a 400 counted as sent")
	}
	r.clock = r.clock.Add(CheckEvery)
	if s.Tick(ctx) {
		t.Fatal("a 400 counted as sent")
	}
	if n := strings.Count(r.logs.String(), "couldn't send usage stats"); n != 1 {
		t.Errorf("logged %d times: %s", n, r.logs)
	}
	r.coll.status = 0
	r.clock = r.clock.Add(CheckEvery)
	if !s.Tick(ctx) || !strings.Contains(r.logs.String(), "usage stats sent") {
		t.Fatalf("not sent after the failure: %s", r.logs)
	}
	// Still the first report: nothing had been received.
	if got := r.coll.last(t); len(got.Payload.Days) != FirstReportDays {
		t.Errorf("%d days", len(got.Payload.Days))
	}
}

// cancelAfter cancels a context once a round trip has its reply.
type cancelAfter struct{ cancel context.CancelFunc }

func (c cancelAfter) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := http.DefaultTransport.RoundTrip(req)
	c.cancel()
	return res, err
}

func TestRecordsAReportTheServiceTookWhileStopping(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := r.sender(false)
	// serve stops right after the service took the report.
	s.o.Client = &http.Client{Transport: cancelAfter{cancel}}
	s.Tick(ctx)
	bg := context.Background()
	for _, k := range []string{keyLastSent, keyFirstSent} {
		if _, ok, err := r.st.Setting(bg, k); err != nil || !ok {
			t.Errorf("%s not recorded: %v", k, err)
		}
	}
	// So the next start doesn't send it again.
	if s.Tick(bg) || len(r.coll.bodies) != 1 {
		t.Errorf("sent again: %d reports", len(r.coll.bodies))
	}
}

func TestFirstDelay(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	mid := func(rg [2]time.Duration) time.Duration { return rg[0] + (rg[1]-rg[0])/2 }
	s := r.sender(false)
	if got := s.FirstDelay(ctx); got != mid(newInstallFirstCheck) {
		t.Errorf("a new install waits %s", got)
	}
	s.Tick(ctx)
	// Restarted in its first day: no earlier than the next hourly report.
	r.clock = now0.Add(10 * time.Minute)
	if got := s.FirstDelay(ctx); got != FirstDaySendAfter-10*time.Minute {
		t.Errorf("a restart in the first day waits %s", got)
	}
	r.clock = now0.Add(FirstDay)
	if got := s.FirstDelay(ctx); got != mid(firstCheck) {
		t.Errorf("after the first day it waits %s", got)
	}
}

func TestRunStopsWithNowhereToSend(t *testing.T) {
	r := newRig(t)
	r.cfg.URL = ""
	done := make(chan struct{})
	go func() {
		r.sender(false).Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't return")
	}
}
