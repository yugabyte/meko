// Package callhome sends Meko's usage stats to YugabyteDB's call home service
// (DefaultURL): a heartbeat of counts once a day, every hour in
// an install's first day, on Kode's schedule (src/extensions/agentk/src/server/callhome.ts). The body
// is yugabyted's envelope with Meko's report as its payload; the service routes it by callhome_type
// to a table of its own.
//
// Only counts, enums and versions are sent, and the install's random id: never a datapack, memory,
// conversation or file's name or content, a query, a path, a host name or a token. An agent_id is
// cut to its client (claude_code:my-repo is claude_code) and matched against a fixed list.
package callhome

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/store"
)

// DefaultURL is YugabyteDB's call home service: release installs send there unless [call_home] url
// names another.
const DefaultURL = "https://diagnostics.meko.yugabyte.com"

// The schedule, Kode's.
const (
	// CheckEvery is how often the sender checks whether a report is due.
	CheckEvery = time.Hour
	// SendAfter is how long after the last report the next is due: just under a day, so hourly
	// checks don't drift later each day.
	SendAfter = 23 * time.Hour
	// FirstDay is how long after its first report an install reports at every check, FirstDaySendAfter
	// apart (just under an hour, for the same reason).
	FirstDay          = 24 * time.Hour
	FirstDaySendAfter = CheckEvery - 5*time.Minute
	// ReportDays is the days in a report; an install's first report has FirstReportDays.
	ReportDays      = 7
	FirstReportDays = 30
	sendTimeout     = 15 * time.Second
)

// The first check after start waits a random time in its range: a new install's (no report yet, or
// its first under a day ago) comes within minutes, others spread out so a release doesn't make every
// install report at once.
var (
	firstCheck           = [2]time.Duration{10 * time.Minute, time.Hour}
	newInstallFirstCheck = [2]time.Duration{time.Minute, 5 * time.Minute}
)

// Settings keys.
const (
	// KeyEnabled is "false" once usage stats are turned off in the web UI.
	KeyEnabled   = "call_home_enabled"
	keyLastSent  = "call_home_last_sent"
	keyFirstSent = "call_home_first_sent"
	keyFirstDay  = "call_home_first_day"
)

// OffByWebUI is State.By when the web UI's setting turned usage stats off.
const OffByWebUI = "web UI"

// Clients are the agent_id prefixes reported by name; the rest are "other", and no agent_id is "none".
var Clients = []string{"claude_code", "claude_desktop", "cursor", "codex", "kiro", "kode", "meko_cli", "meko_web_ui"}

// Install is what the report says about the install.
type Install struct {
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	Platform string `json:"platform"`
	Arch     string `json:"arch"`
	Go       string `json:"go"`
	Service  string `json:"service"` // launchd, systemd or none
	Auth     string `json:"auth"`    // token or none
	Secrets  string `json:"secrets"` // keychain or file
	Update   Update `json:"update"`
	// ContextEngine is the context engine's own state when it answers (loading, ready or error), else
	// the server's view of it (starting, restarting, failed, or unavailable when it can't start).
	ContextEngine string          `json:"contextEngine"`
	Extensions    map[string]bool `json:"extensions"` // the known extensions, installed or not
	FirstDay      string          `json:"firstDay"`   // the UTC day of the first report
}

// Update is the install's update settings.
type Update struct {
	Channel string `json:"channel"`
	Auto    bool   `json:"auto"`
}

// Totals is what the install holds.
type Totals struct {
	Datapacks        int   `json:"datapacks"`
	Memories         int   `json:"memories"`
	Learnings        int   `json:"learnings"`
	PromotedMemories int   `json:"promotedMemories"`
	Artifacts        int   `json:"artifacts"`
	Conversations    int   `json:"conversations"`
	KBDocuments      int   `json:"kbDocuments"`
	KBBytes          int64 `json:"kbBytes"`
	DBBytes          int64 `json:"dbBytes"`
}

// Day is one UTC day's MCP calls.
type Day struct {
	Day         string         `json:"day"`
	Calls       int            `json:"calls"`
	Errors      int            `json:"errors"`
	Ms          int64          `json:"ms"`
	ByTool      map[string]int `json:"byTool"`
	ByClient    map[string]int `json:"byClient"`
	TokensSent  int64          `json:"tokensSent"`
	TokensSaved int64          `json:"tokensSaved"`
	ContextGaps ContextGaps    `json:"contextGaps"`
}

// ContextGaps is one day's searches Meko answered poorly, by how (found nothing, or nothing
// relevant), and the questions people dismissed and taught on Context Gaps. Never a query.
type ContextGaps struct {
	Empty     int `json:"empty"`
	Weak      int `json:"weak"`
	Dismissed int `json:"dismissed"`
	Taught    int `json:"taught"`
}

// Report is the payload.
type Report struct {
	Schema  int     `json:"schema"`
	Install Install `json:"install"`
	Totals  Totals  `json:"totals"`
	Days    []Day   `json:"days"` // oldest first
}

// Envelope is the body: yugabyted's call home fields, with the report as the payload.
type Envelope struct {
	CallhomeType string `json:"callhome_type"` // meko
	Command      string `json:"command"`       // heartbeat
	// Timestamp is UTC with microseconds and no zone (2026-11-09T17:05:32.123456): the only form the
	// service parses; it drops a row it can't parse and still answers 200.
	Timestamp string `json:"timestamp"`
	InstallID string `json:"install_id"`
	Version   string `json:"version"`
	Payload   Report `json:"payload"`
}

// Timestamp formats a time as the service parses it.
func Timestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000")
}

// Client is the client an agent_id names, as the report has it.
func Client(agentID string) string {
	c, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(agentID)), ":")
	switch c {
	case "", "meko_agent":
		return "none"
	case "agent-k-local", "agentk": // the agent_id Kode has always sent, and its name in Meko before
		return "kode"
	}
	for _, k := range Clients {
		if c == k {
			return c
		}
	}
	return "other"
}

// Options configure a Sender.
type Options struct {
	Store  *store.Store
	Config config.CallHome
	// Release is true for a release install, which sends to DefaultURL when Config has no URL.
	Release bool
	// Facts is what the report says about the install, but its first day.
	Facts func(context.Context) Install
	// Tools are the MCP tools reported by name; the rest are "other".
	Tools  []string
	Log    *slog.Logger
	Client *http.Client
	Now    func() time.Time
	Rand   func() float64
	// FirstCheck, when set, replaces Run's random first delay (for tests).
	FirstCheck time.Duration
}

// Sender sends the report while usage stats are on and there's somewhere to send it.
type Sender struct {
	o       Options
	tools   map[string]bool
	failing bool // the last send failed: failures are logged once until a send works again
}

// New makes a Sender.
func New(o Options) *Sender {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: sendTimeout}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Rand == nil {
		o.Rand = rand.Float64
	}
	if o.Facts == nil {
		o.Facts = func(context.Context) Install { return Install{} }
	}
	tools := map[string]bool{}
	for _, t := range o.Tools {
		tools[t] = true
	}
	return &Sender{o: o, tools: tools}
}

// Endpoint is where reports go: [call_home] url, else DefaultURL from a release install; "" when
// there's neither.
func (s *Sender) Endpoint() string {
	if s.o.Config.URL != "" {
		return s.o.Config.URL
	}
	if s.o.Release {
		return DefaultURL
	}
	return ""
}

// State is whether usage stats are on, as status and the web UI show it.
type State struct {
	Enabled bool `json:"enabled"`
	// ForcedOff is true when config or the environment turned them off: the web UI can't turn them on.
	ForcedOff bool `json:"forced_off"`
	// By says what turned them off: OffByWebUI, or config's OffBy.
	By string `json:"by,omitempty"`
	// Sends is false when they're on but there's nowhere to send them (a build from source).
	Sends bool `json:"sends"`
	// URL is where they go while Sends: DefaultURL, or the one config names.
	URL string `json:"url,omitempty"`
}

// State reads whether usage stats are on.
func (s *Sender) State(ctx context.Context) (State, error) {
	if !s.o.Config.Enabled {
		return State{ForcedOff: true, By: s.o.Config.OffBy}, nil
	}
	v, _, err := s.o.Store.Setting(ctx, KeyEnabled)
	if err != nil {
		return State{}, err
	}
	if v == "false" {
		return State{By: OffByWebUI}, nil
	}
	url := s.Endpoint()
	return State{Enabled: true, Sends: url != "", URL: url}, nil
}

// SetEnabled is the web UI's setting. It takes effect at the next check, with no restart.
func (s *Sender) SetEnabled(ctx context.Context, on bool) (State, error) {
	if err := s.o.Store.SetSetting(ctx, KeyEnabled, strconv.FormatBool(on)); err != nil {
		return State{}, err
	}
	return s.State(ctx)
}

func (s *Sender) settingTime(ctx context.Context, key string) time.Time {
	v, ok, err := s.o.Store.Setting(ctx, key)
	if err != nil || !ok {
		return time.Time{}
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func (s *Sender) setTime(ctx context.Context, key string, t time.Time) error {
	return s.o.Store.SetSetting(ctx, key, strconv.FormatInt(t.UnixMilli(), 10))
}

// firstDay is true until a day after the first report: no report yet, or the first one under
// FirstDay ago.
func (s *Sender) firstDay(ctx context.Context, last, now time.Time) bool {
	if last.IsZero() {
		return true
	}
	first := s.settingTime(ctx, keyFirstSent)
	return !first.IsZero() && now.Sub(first) < FirstDay
}

// envelope is what the next send carries.
func (s *Sender) envelope(ctx context.Context) (Envelope, error) {
	now := s.o.Now()
	firstDay, ok, err := s.o.Store.Setting(ctx, keyFirstDay)
	if err != nil {
		return Envelope{}, err
	}
	if !ok {
		firstDay = now.UTC().Format(time.DateOnly)
		if err = s.o.Store.SetSetting(ctx, keyFirstDay, firstDay); err != nil {
			return Envelope{}, err
		}
	}
	installID, err := s.o.Store.EnsureInstallID(ctx)
	if err != nil {
		return Envelope{}, err
	}
	days := ReportDays
	if s.settingTime(ctx, keyLastSent).IsZero() {
		days = FirstReportDays
	}
	r, err := s.report(ctx, now, days)
	if err != nil {
		return Envelope{}, err
	}
	r.Install.FirstDay = firstDay
	return Envelope{CallhomeType: "meko", Command: "heartbeat", Timestamp: Timestamp(now),
		InstallID: installID, Version: r.Install.Version, Payload: r}, nil
}

// report builds the report of the days UTC days ending today.
func (s *Sender) report(ctx context.Context, now time.Time, days int) (Report, error) {
	t, err := s.o.Store.UsageTotals(ctx)
	if err != nil {
		return Report{}, err
	}
	today := now.UTC().Truncate(24 * time.Hour)
	start := today.AddDate(0, 0, -(days - 1))
	rows, err := s.o.Store.CallDays(ctx, start)
	if err != nil {
		return Report{}, err
	}
	out := make([]Day, days)
	index := map[string]*Day{}
	for i := range out {
		d := start.AddDate(0, 0, i).Format(time.DateOnly)
		out[i] = Day{Day: d, ByTool: map[string]int{}, ByClient: map[string]int{}}
		index[d] = &out[i]
	}
	for _, r := range rows {
		d := index[r.Day]
		if d == nil {
			continue
		}
		tool := r.Tool
		if !s.tools[tool] {
			tool = "other"
		}
		d.Calls += r.Calls
		d.Errors += r.Errors
		d.Ms += r.Ms
		d.TokensSent += r.TokensSent
		d.TokensSaved += r.TokensSaved
		d.ByTool[tool] += r.Calls
		d.ByClient[Client(r.AgentID)] += r.Calls
	}
	// Context Gaps' counts are an extra: a failure to read them leaves them at 0, not the report unsent.
	gaps, _ := s.o.Store.QuestionDays(ctx, start)
	for _, g := range gaps {
		if d := index[g.Day]; d != nil {
			d.ContextGaps = ContextGaps{Empty: g.Empty, Weak: g.Weak, Dismissed: g.Dismissed, Taught: g.Taught}
		}
	}
	install := s.o.Facts(ctx)
	if install.Extensions == nil {
		install.Extensions = map[string]bool{}
	}
	return Report{Schema: 3, Install: install, Totals: Totals{Datapacks: t.Datapacks, Memories: t.Memories,
		Learnings: t.Learnings, PromotedMemories: t.PromotedMemories, Artifacts: t.Artifacts,
		Conversations: t.Conversations, KBDocuments: t.KBDocuments, KBBytes: t.KBBytes, DBBytes: t.DBBytes},
		Days: out}, nil
}

// Tick sends a report if one is due. It never fails; true when one was sent.
func (s *Sender) Tick(ctx context.Context) bool {
	url := s.Endpoint()
	if url == "" {
		return false
	}
	if st, err := s.State(ctx); err != nil || !st.Enabled {
		return false
	}
	// Reports are stamped with the check's time, not the send's, so the time a send takes doesn't
	// push the next hourly check under FirstDaySendAfter.
	now := s.o.Now()
	last := s.settingTime(ctx, keyLastSent)
	after := SendAfter
	if s.firstDay(ctx, last, now) {
		after = FirstDaySendAfter
	}
	if !last.IsZero() && now.Sub(last) < after {
		return false
	}
	err := s.send(ctx, url, now, last.IsZero())
	if err != nil {
		if !s.failing && ctx.Err() == nil {
			s.o.Log.Warn("couldn't send usage stats; trying again every hour", "error", err)
		}
		s.failing = true
		return false
	}
	if s.failing {
		s.o.Log.Info("usage stats sent")
	}
	s.failing = false
	return true
}

func (s *Sender) send(ctx context.Context, url string, now time.Time, first bool) error {
	e, err := s.envelope(ctx)
	if err != nil {
		return err
	}
	if err = s.post(ctx, url, e); err != nil {
		return err
	}
	// A report the service took is recorded even when serve is stopping, or the next start would
	// send it again (as a first report, if it was one).
	rec := context.WithoutCancel(ctx)
	if first {
		if err = s.setTime(rec, keyFirstSent, now); err != nil {
			return err
		}
	}
	return s.setTime(rec, keyLastSent, now)
}

// post sends the envelope, giving up after sendTimeout.
func (s *Sender) post(ctx context.Context, url string, e Envelope) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "meko/"+e.Version)
	res, err := s.o.Client.Do(req)
	if err != nil {
		return err
	}
	_ = res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("the call home service answered %d", res.StatusCode)
	}
	return nil
}

// FirstDelay is how long Run waits before its first check: a new install's is short, and no
// earlier than its next hourly report is due, so a restart in its first day doesn't skip one.
func (s *Sender) FirstDelay(ctx context.Context) time.Duration {
	now := s.o.Now()
	last := s.settingTime(ctx, keyLastSent)
	first := s.firstDay(ctx, last, now)
	r := firstCheck
	if first {
		r = newInstallFirstCheck
	}
	delay := r[0] + time.Duration(s.o.Rand()*float64(r[1]-r[0]))
	if due := last.Add(FirstDaySendAfter).Sub(now); first && !last.IsZero() && due > delay {
		delay = due
	}
	return delay
}

// Run checks after FirstDelay, then every CheckEvery, until ctx ends. It returns at once when there's
// nowhere to send reports.
func (s *Sender) Run(ctx context.Context) {
	if s.Endpoint() == "" {
		return
	}
	delay := s.o.FirstCheck
	if delay == 0 {
		delay = s.FirstDelay(ctx)
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.Tick(ctx)
		t.Reset(CheckEvery)
	}
}
