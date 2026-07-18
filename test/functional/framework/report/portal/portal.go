// Package portal streams a run to the amp report portal's live write API as
// it executes: the run appears on the portal within seconds of starting, each
// test shows up when it starts, and steps/metrics/perf/charts arrive as they
// are emitted. The finished result is normalized server-side (the PATCH
// carries the raw Result under "amp"), so live-streamed and post-run-ingested
// runs store identically.
//
// The sink is strictly non-critical: a portal outage must never fail a run.
// The first error of each request kind is logged, later ones are counted
// silently, and a failed run-open disables the sink for the rest of the run.
package portal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// Meta is the run-identity stamped onto the portal run row (all optional).
type Meta struct {
	// RunID adopts an EXISTING portal run (created at trigger time) instead
	// of opening a new one — the trigger row and the live-streamed run are
	// the same object.
	RunID       string
	Report      string // report bucket the run files under
	Branch      string
	CommitSHA   string
	BuildTag    string
	TriggerType string
	TriggeredBy string
}

// Sink implements core.Sink over the portal's live HTTP API. All methods run
// on the reporter's single per-sink worker goroutine, so state needs no lock.
type Sink struct {
	base string
	hc   *http.Client
	meta Meta

	runID   string
	runTs   string            // file-ref prefix for uploaded attachments
	results map[string]string // test name -> portal result id
	ordinal map[string]int    // test name -> next step ordinal
	steps   map[*core.Step]string
	errOnce map[string]bool
	dead    bool // run-open failed; every later event is a no-op
}

// New builds a Sink against the portal base URL (scheme://host[:port]).
func New(baseURL string, meta Meta) *Sink {
	return &Sink{
		base:    strings.TrimRight(baseURL, "/"),
		hc:      &http.Client{Timeout: 30 * time.Second},
		meta:    meta,
		results: map[string]string{},
		ordinal: map[string]int{},
		steps:   map[*core.Step]string{},
		errOnce: map[string]bool{},
	}
}

// RunStarted opens the portal run. Failure disables the sink for this run.
func (s *Sink) RunStarted(info core.RunInfo) error {
	s.runTs = info.StartedAt.UTC().Format("20060102-150405")
	data := map[string]any{
		"seed":     info.Seed,
		"total":    info.Total,
		"parallel": info.Parallel,
	}
	stamp(data, "report", s.meta.Report)
	stamp(data, "branch", s.meta.Branch)
	stamp(data, "commit_sha", s.meta.CommitSHA)
	stamp(data, "build_tag", s.meta.BuildTag)
	stamp(data, "trigger_type", s.meta.TriggerType)
	stamp(data, "triggered_by", s.meta.TriggeredBy)

	if s.meta.RunID != "" {
		// Adopt the trigger-created run: flip it to running and stamp identity.
		err := s.call(http.MethodPatch, "/api/v1/runs/"+s.meta.RunID, map[string]any{
			"status": "running", "start_time": info.StartedAt, "data": data,
		}, nil)
		if err != nil {
			s.dead = true
			log.Printf("portal sink: adopt run %s failed, live reporting disabled for this run: %v", s.meta.RunID, err)
			return nil
		}
		s.runID = s.meta.RunID
		return nil
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	err := s.call(http.MethodPost, "/api/v1/runs", map[string]any{
		"status": "running", "start_time": info.StartedAt, "data": data,
	}, &out)
	if err != nil || out.RunID == "" {
		s.dead = true
		log.Printf("portal sink: open run failed, live reporting disabled for this run: %v", err)
		return nil
	}
	s.runID = out.RunID
	return nil
}

// ResultEvent streams one live event.
func (s *Sink) ResultEvent(ev core.ResultEvent) error {
	if s.dead {
		return nil
	}
	switch ev.Kind {
	case core.EventTestStarted:
		s.testStarted(ev)
	case core.EventTestFinished:
		s.testFinished(ev)
	case core.EventStepStarted:
		s.stepStarted(ev)
	case core.EventStepFinished:
		s.stepFinished(ev)
	case core.EventAttachment:
		s.attachment(ev)
	case core.EventMetric:
		s.metric(ev)
	case core.EventDatapoint:
		s.datapoint(ev)
	case core.EventChart:
		s.chart(ev)
	}
	return nil
}

// RunFinished closes the portal run.
func (s *Sink) RunFinished(sum core.RunSummary) error {
	if s.dead {
		return nil
	}
	status := "passed"
	counts := map[string]int{}
	for st, n := range sum.Counts {
		counts[string(st)] = n
		if n > 0 && st != core.StatusPassed && st != core.StatusSkipped {
			status = "failed"
		}
	}
	_ = s.call(http.MethodPatch, "/api/v1/runs/"+s.runID, map[string]any{
		"status": status, "end_time": sum.EndedAt,
		"data": map[string]any{"counts": counts, "total": sum.Total},
	}, nil)
	return nil
}

func (s *Sink) testStarted(ev core.ResultEvent) {
	body := map[string]any{"name": ev.TestName, "status": "running"}
	if ev.Result != nil && !ev.Result.StartedAt.IsZero() {
		body["start_time"] = ev.Result.StartedAt
	}
	var out struct {
		ResultID string `json:"result_id"`
	}
	if err := s.call(http.MethodPost, "/api/v1/runs/"+s.runID+"/results", body, &out); err == nil {
		s.results[ev.TestName] = out.ResultID
	}
}

// testFinished patches status/end and ships the raw Result under "amp": the
// server normalizes it with the same mapping the bulk ingest path uses, so
// live and ingested result data are shaped identically.
func (s *Sink) testFinished(ev core.ResultEvent) {
	rid := s.results[ev.TestName]
	if rid == "" || ev.Result == nil {
		return
	}
	r := ev.Result
	raw, err := json.Marshal(resultView{
		Result:       r,
		CategoryName: r.Category.String(),
		DurationMS:   r.Duration.Milliseconds(),
	})
	if err != nil {
		raw = []byte(`{}`)
	}
	_ = s.call(http.MethodPatch, "/api/v1/results/"+rid, map[string]any{
		"status": string(r.Status), "end_time": r.EndedAt, "amp": json.RawMessage(raw),
	}, nil)
}

// resultView mirrors the jsonfile sink's on-disk shape — the one the portal's
// ingest mapping already understands.
type resultView struct {
	*core.Result
	CategoryName string `json:"category_name"`
	DurationMS   int64  `json:"duration_ms"`
}

func (s *Sink) stepStarted(ev core.ResultEvent) {
	rid := s.results[ev.TestName]
	if rid == "" || ev.Step == nil {
		return
	}
	ord := s.ordinal[ev.TestName]
	s.ordinal[ev.TestName] = ord + 1
	props := map[string]any{"depth": ev.Step.Depth, "step_name": ev.Step.Name}
	for k, v := range ev.Step.Properties {
		props[k] = v
	}
	var out struct {
		StepID string `json:"step_id"`
	}
	body := map[string]any{
		"ordinal": ord, "status": "running",
		"start_time": ev.Step.StartedAt, "properties": props,
	}
	if err := s.call(http.MethodPost, "/api/v1/results/"+rid+"/steps", body, &out); err == nil {
		s.steps[ev.Step] = out.StepID
	}
}

func (s *Sink) stepFinished(ev core.ResultEvent) {
	if ev.Step == nil {
		return
	}
	sid := s.steps[ev.Step]
	if sid == "" {
		return
	}
	delete(s.steps, ev.Step)
	_ = s.call(http.MethodPatch, "/api/v1/steps/"+sid, map[string]any{
		"status": string(ev.Step.Status), "end_time": ev.Step.EndedAt,
	}, nil)
}

func (s *Sink) attachment(ev core.ResultEvent) {
	rid := s.results[ev.TestName]
	if rid == "" || ev.Attachment == nil {
		return
	}
	a := ev.Attachment
	source := ""
	if len(ev.Content) > 0 {
		ref := path.Join(s.runTs, safe(ev.TestName), safe(a.Name))
		if err := s.upload(ref, a.Name, ev.Content); err != nil {
			s.logOnce("upload", err)
		} else {
			source = ref
		}
	}
	data, _ := json.Marshal(map[string]any{"size": a.Size, "step_name": a.StepName})
	_ = s.call(http.MethodPost, "/api/v1/attachments", map[string]any{
		"parent_id": rid, "parent_kind": "result",
		"name": a.Name, "source": source, "type": a.Kind,
		"data": json.RawMessage(data),
	}, nil)
}

func (s *Sink) metric(ev core.ResultEvent) {
	rid := s.results[ev.TestName]
	if rid == "" || ev.Metric == nil {
		return
	}
	_ = s.call(http.MethodPost, "/api/v1/results/"+rid+"/metrics", []map[string]any{{
		"key": ev.Metric.Key, "value": ev.Metric.Value, "unit": ev.Metric.Unit,
	}}, nil)
}

func (s *Sink) datapoint(ev core.ResultEvent) {
	rid := s.results[ev.TestName]
	if rid == "" || ev.Datapoint == nil {
		return
	}
	dp, err := json.Marshal(ev.Datapoint)
	if err != nil {
		return
	}
	_ = s.call(http.MethodPost, "/api/v1/results/"+rid+"/perf", map[string]any{
		"datapoints": []json.RawMessage{dp},
	}, nil)
}

func (s *Sink) chart(ev core.ResultEvent) {
	rid := s.results[ev.TestName]
	if rid == "" || ev.Chart == nil {
		return
	}
	c, err := json.Marshal(ev.Chart)
	if err != nil {
		return
	}
	_ = s.call(http.MethodPost, "/api/v1/results/"+rid+"/charts", []json.RawMessage{c}, nil)
}

// call posts/patches JSON and decodes the response into out (when non-nil).
func (s *Sink) call(method, p string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(method, s.base+p, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		s.logOnce(method+" "+p, err)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		err := fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
		s.logOnce(method+" "+p, err)
		return err
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// upload sends attachment bytes to the files service behind the same host.
func (s *Sink) upload(ref, name string, content []byte) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", name)
	if err != nil {
		return err
	}
	if _, err := part.Write(content); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, s.base+"/api/v1/files/add", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("name", ref)
	resp, err := s.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// logOnce logs the first error per request kind; repeats stay silent (a dead
// portal would otherwise flood the run output).
func (s *Sink) logOnce(kind string, err error) {
	if s.errOnce[kind] {
		return
	}
	s.errOnce[kind] = true
	log.Printf("portal sink: %s: %v (further errors of this kind are silent)", kind, err)
}

func stamp(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}

func safe(v string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", " ", "_")
	return r.Replace(v)
}
