// Package jsonfile is a Sink that dumps a run to a directory: one JSON file per
// test result (full result incl. steps, seed, resolved config, versions, error),
// an incremental manifest.jsonl (one line per finished test — the resume
// groundwork), and a run.json summary at the end. It never buffers step data:
// per-result files are written as each test finishes.
package jsonfile

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// Sink writes run artifacts under baseDir/<runid>.
type Sink struct {
	baseDir string

	mu       sync.Mutex
	dir      string
	manifest *os.File
	info     core.RunInfo
	summary  []summaryRow
	err      error
	// artifacts maps a test name → (attachment name → run-relative path) for the
	// bytes this sink persisted from EventAttachment events, so the run-relative
	// Path is recorded into the test's on-disk result at EventTestFinished
	// without mutating the shared (cross-sink) Result.
	artifacts map[string]map[string]string
}

type summaryRow struct {
	Name       string `json:"name"`
	Suite      string `json:"suite"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Seed       int64  `json:"seed"`
}

// New returns a Sink writing under baseDir. The per-run subdirectory
// (baseDir/<runid>) is created when the run starts.
func New(baseDir string) *Sink { return &Sink{baseDir: baseDir} }

// Dir is the resolved per-run output directory (valid after RunStarted).
func (s *Sink) Dir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dir
}

// RunStarted implements core.Sink: it resolves and creates the output directory
// and opens the incremental manifest.
func (s *Sink) RunStarted(info core.RunInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.info = info
	return s.ensureDirLocked()
}

func (s *Sink) ensureDirLocked() error {
	if s.dir != "" {
		return nil
	}
	ts := s.info.StartedAt
	if ts.IsZero() {
		ts = time.Now()
	}
	runID := fmt.Sprintf("%s-%d", ts.UTC().Format("20060102-150405"), s.info.Seed)
	s.dir = filepath.Join(s.baseDir, runID)
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		s.err = err
		return err
	}
	f, err := os.Create(filepath.Join(s.dir, "manifest.jsonl"))
	if err != nil {
		s.err = err
		return err
	}
	s.manifest = f
	return nil
}

// ResultEvent implements core.Sink. On EventAttachment carrying bytes it writes
// them under the run's artifacts dir; on EventTestFinished it writes the full
// result file (with any persisted-artifact paths) and appends a manifest line.
func (s *Sink) ResultEvent(ev core.ResultEvent) error {
	switch ev.Kind {
	case core.EventAttachment:
		return s.writeAttachment(ev)
	case core.EventTestFinished:
		if ev.Result == nil {
			return nil
		}
	default:
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDirLocked(); err != nil {
		return err
	}
	r := ev.Result
	data, err := json.MarshalIndent(s.newResultViewLocked(r), "", "  ")
	if err != nil {
		return err
	}
	delete(s.artifacts, r.TestName)
	if err := os.WriteFile(filepath.Join(s.dir, safeName(r.TestName)+".json"), data, 0o644); err != nil {
		return err
	}

	row := summaryRow{
		Name:       r.TestName,
		Suite:      r.Suite,
		Status:     string(r.Status),
		DurationMS: r.Duration.Milliseconds(),
		Seed:       r.Seed,
	}
	s.summary = append(s.summary, row)
	if s.manifest != nil {
		line, _ := json.Marshal(row)
		if _, err := s.manifest.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// RunFinished implements core.Sink: it writes run.json and closes the manifest.
func (s *Sink) RunFinished(sum core.RunSummary) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDirLocked(); err != nil {
		return err
	}
	counts := map[string]int{}
	for st, n := range sum.Counts {
		counts[string(st)] = n
	}
	run := map[string]any{
		"seed":       s.info.Seed,
		"parallel":   s.info.Parallel,
		"total":      sum.Total,
		"counts":     counts,
		"started_at": sum.StartedAt.UTC(),
		"ended_at":   sum.EndedAt.UTC(),
		"tests":      s.summary,
	}
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.dir, "run.json"), data, 0o644); err != nil {
		return err
	}
	if s.manifest != nil {
		err = s.manifest.Close()
		s.manifest = nil
	}
	return err
}

// writeAttachment persists an attachment's bytes (from an EventAttachment carrying
// Content) to <dir>/artifacts/<test>/<name> and records the run-relative path so
// EventTestFinished can stamp it onto the recorded result. Metadata-only attaches
// (nil Content) are ignored here — they carry no bytes to write.
func (s *Sink) writeAttachment(ev core.ResultEvent) error {
	if ev.Attachment == nil || len(ev.Content) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDirLocked(); err != nil {
		return err
	}
	safeTest := safeName(ev.TestName)
	safeAttach := safeName(ev.Attachment.Name)
	dir := filepath.Join(s.dir, "artifacts", safeTest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, safeAttach), ev.Content, 0o644); err != nil {
		return err
	}
	if s.artifacts == nil {
		s.artifacts = map[string]map[string]string{}
	}
	if s.artifacts[ev.TestName] == nil {
		s.artifacts[ev.TestName] = map[string]string{}
	}
	s.artifacts[ev.TestName][ev.Attachment.Name] = path.Join("artifacts", safeTest, safeAttach)
	return nil
}

// resultView is the on-disk shape of a Result — the raw Result plus the
// human-readable enum strings the numeric Category/Status would otherwise hide.
// Attachments shadows the embedded Result's field (shallower depth wins in
// encoding/json) so the persisted paths land on disk without mutating the
// cross-sink Result.
type resultView struct {
	*core.Result
	CategoryName string            `json:"category_name"`
	DurationMS   int64             `json:"duration_ms"`
	Attachments  []core.Attachment `json:"Attachments"`
}

// newResultViewLocked builds the on-disk view, filling each attachment's Path
// from the bytes this sink already persisted for the test. Callers hold s.mu.
func (s *Sink) newResultViewLocked(r *core.Result) resultView {
	paths := s.artifacts[r.TestName]
	atts := make([]core.Attachment, len(r.Attachments))
	for i, a := range r.Attachments {
		if p, ok := paths[a.Name]; ok {
			a.Path = p
		}
		atts[i] = a
	}
	return resultView{Result: r, CategoryName: r.Category.String(), DurationMS: r.Duration.Milliseconds(), Attachments: atts}
}

func safeName(name string) string {
	repl := strings.NewReplacer("/", "_", " ", "_", string(os.PathSeparator), "_")
	return repl.Replace(name)
}
