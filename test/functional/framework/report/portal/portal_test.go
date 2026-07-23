package portal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// fakePortal records every live-API call and hands out deterministic ids.
type fakePortal struct {
	mu    sync.Mutex
	calls []string // "METHOD path"
	body  map[string]json.RawMessage
}

func newFakePortal(t *testing.T) (*fakePortal, *httptest.Server) {
	t.Helper()
	fp := &fakePortal{body: map[string]json.RawMessage{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := json.Marshal(map[string]any{})
		if r.Body != nil {
			b := make([]byte, 0, 1024)
			buf := make([]byte, 1024)
			for {
				n, err := r.Body.Read(buf)
				b = append(b, buf[:n]...)
				if err != nil {
					break
				}
			}
			raw = b
		}
		fp.mu.Lock()
		key := r.Method + " " + r.URL.Path
		fp.calls = append(fp.calls, key)
		fp.body[key] = raw
		fp.mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v1/runs":
			_ = json.NewEncoder(w).Encode(map[string]string{"run_id": "run-1"})
		case r.Method == "POST" && r.URL.Path == "/api/v1/runs/run-1/results":
			_ = json.NewEncoder(w).Encode(map[string]string{"result_id": "res-1"})
		case r.Method == "POST" && r.URL.Path == "/api/v1/results/res-1/steps":
			_ = json.NewEncoder(w).Encode(map[string]string{"step_id": "step-1"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		}
	}))
	t.Cleanup(srv.Close)
	return fp, srv
}

func (fp *fakePortal) has(t *testing.T, call string) json.RawMessage {
	t.Helper()
	fp.mu.Lock()
	defer fp.mu.Unlock()
	raw, ok := fp.body[call]
	if !ok {
		t.Fatalf("call %q missing; got %v", call, fp.calls)
	}
	return raw
}

func TestSinkStreamsFullLifecycle(t *testing.T) {
	fp, srv := newFakePortal(t)
	s := New(srv.URL, Meta{
		Report: "regression", Branch: "main", CommitSHA: "abc123",
		BuildTag: "custom-20260717-9", TriggerType: "jenkins", TriggeredBy: "portal",
	})

	start := time.Now()
	_ = s.RunStarted(core.RunInfo{Seed: 7, Total: 1, Parallel: 2, StartedAt: start})

	res := &core.Result{TestName: "features/x", Status: core.StatusPassed,
		StartedAt: start, EndedAt: start.Add(time.Minute), Duration: time.Minute}
	step := &core.Step{Name: "deploy", Status: core.StatusPassed, StartedAt: start, EndedAt: start.Add(time.Second)}

	_ = s.ResultEvent(core.ResultEvent{Kind: core.EventTestStarted, TestName: "features/x", Result: res})
	_ = s.ResultEvent(core.ResultEvent{Kind: core.EventStepStarted, TestName: "features/x", Step: step})
	_ = s.ResultEvent(core.ResultEvent{Kind: core.EventStepFinished, TestName: "features/x", Step: step})
	_ = s.ResultEvent(core.ResultEvent{Kind: core.EventMetric, TestName: "features/x",
		Metric: &core.Metric{Key: "rows", Value: 100, Unit: "n"}})
	_ = s.ResultEvent(core.ResultEvent{Kind: core.EventAttachment, TestName: "features/x",
		Attachment: &core.Attachment{Name: "run.log", Kind: "log", Size: 2}, Content: []byte("ok")})
	_ = s.ResultEvent(core.ResultEvent{Kind: core.EventTestFinished, TestName: "features/x", Result: res})
	_ = s.RunFinished(core.RunSummary{Counts: map[core.Status]int{core.StatusPassed: 1},
		Total: 1, StartedAt: start, EndedAt: start.Add(time.Minute)})

	var run struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(fp.has(t, "POST /api/v1/runs"), &run); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"report", "branch", "commit_sha", "build_tag", "trigger_type", "triggered_by"} {
		if run.Data[k] == "" || run.Data[k] == nil {
			t.Errorf("run data missing %q: %v", k, run.Data)
		}
	}

	var fin struct {
		Status string          `json:"status"`
		Amp    json.RawMessage `json:"amp"`
	}
	if err := json.Unmarshal(fp.has(t, "PATCH /api/v1/results/res-1"), &fin); err != nil {
		t.Fatal(err)
	}
	if fin.Status != "passed" || len(fin.Amp) == 0 {
		t.Errorf("finish patch = %+v, want passed with amp payload", fin)
	}
	var amp struct {
		DurationMS int64 `json:"duration_ms"`
	}
	_ = json.Unmarshal(fin.Amp, &amp)
	if amp.DurationMS != time.Minute.Milliseconds() {
		t.Errorf("amp duration_ms = %d", amp.DurationMS)
	}

	fp.has(t, "PATCH /api/v1/steps/step-1")
	fp.has(t, "POST /api/v1/results/res-1/metrics")
	fp.has(t, "POST /api/v1/files/add")
	fp.has(t, "POST /api/v1/attachments")

	var done struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(fp.has(t, "PATCH /api/v1/runs/run-1"), &done); err != nil {
		t.Fatal(err)
	}
	if done.Status != "passed" {
		t.Errorf("final run status = %q, want passed", done.Status)
	}
}

func TestSinkFailedRunStatus(t *testing.T) {
	fp, srv := newFakePortal(t)
	s := New(srv.URL, Meta{})
	_ = s.RunStarted(core.RunInfo{StartedAt: time.Now()})
	_ = s.RunFinished(core.RunSummary{
		Counts: map[core.Status]int{core.StatusPassed: 1, core.StatusFailed: 1}, Total: 2, EndedAt: time.Now(),
	})
	var done struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(fp.has(t, "PATCH /api/v1/runs/run-1"), &done); err != nil {
		t.Fatal(err)
	}
	if done.Status != "failed" {
		t.Errorf("final run status = %q, want failed", done.Status)
	}
}

func TestSinkSurvivesDeadPortal(t *testing.T) {
	s := New("http://127.0.0.1:1", Meta{})
	_ = s.RunStarted(core.RunInfo{StartedAt: time.Now()})
	if !s.dead {
		t.Fatal("sink should disable itself when the run cannot be opened")
	}
	// Every later event must be a silent no-op, not a hang or panic.
	_ = s.ResultEvent(core.ResultEvent{Kind: core.EventTestStarted, TestName: "x", Result: &core.Result{}})
	_ = s.RunFinished(core.RunSummary{EndedAt: time.Now()})
}

// With Meta.RunID set the sink adopts the trigger-created run (PATCH, no
// second run object) and streams results into it.
func TestSinkAdoptsExistingRun(t *testing.T) {
	fp, srv := newFakePortal(t)
	s := New(srv.URL, Meta{RunID: "run-1", Report: "regression"})
	_ = s.RunStarted(core.RunInfo{StartedAt: time.Now()})
	res := &core.Result{TestName: "x", Status: core.StatusPassed}
	_ = s.ResultEvent(core.ResultEvent{Kind: core.EventTestStarted, TestName: "x", Result: res})

	fp.has(t, "PATCH /api/v1/runs/run-1")
	fp.has(t, "POST /api/v1/runs/run-1/results")
	fp.mu.Lock()
	for _, c := range fp.calls {
		if c == "POST /api/v1/runs" {
			t.Error("sink opened a second run despite RunID being set")
		}
	}
	fp.mu.Unlock()
}

// An attachment emitted AFTER the test's finish event (teardown/report-kill
// blobs race the finish on the reporter queue) must still land on the test's
// result — the sink keeps the result id past testFinished.
func TestSinkAttachmentAfterTestFinished(t *testing.T) {
	fp, srv := newFakePortal(t)
	s := New(srv.URL, Meta{})
	_ = s.RunStarted(core.RunInfo{StartedAt: time.Now()})

	res := &core.Result{TestName: "features/x", Status: core.StatusFailed}
	_ = s.ResultEvent(core.ResultEvent{Kind: core.EventTestStarted, TestName: "features/x", Result: res})
	_ = s.ResultEvent(core.ResultEvent{Kind: core.EventTestFinished, TestName: "features/x", Result: res})
	_ = s.ResultEvent(core.ResultEvent{Kind: core.EventAttachment, TestName: "features/x",
		Attachment: &core.Attachment{Name: "late.log", Kind: "log", Size: 4}, Content: []byte("late")})

	var att struct {
		ParentID string `json:"parent_id"`
		Source   string `json:"source"`
	}
	if err := json.Unmarshal(fp.has(t, "POST /api/v1/attachments"), &att); err != nil {
		t.Fatal(err)
	}
	if att.ParentID != "res-1" {
		t.Errorf("late attachment parent_id = %q, want res-1", att.ParentID)
	}
	if att.Source == "" {
		t.Errorf("late attachment source empty — bytes were not uploaded")
	}
	fp.has(t, "POST /api/v1/files/add")
}
