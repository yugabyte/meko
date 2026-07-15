package html

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/yugabyte/yb-amp/test/system/framework/core"
)

// Sink is a live core.Sink that accumulates finished results in memory and
// renders the self-contained HTML report to path at RunFinished — the same
// shape as report/junit.Sink. It is meant for short/bounded runs (an operator
// asking for `ampqa run --html report.html`); for anything long enough to
// worry about memory, generate the report after the fact from the jsonfile
// output via `ampqa report --from <out-dir>` instead.
type Sink struct {
	path string

	mu      sync.Mutex
	info    core.RunInfo
	results []*core.Result
}

// New returns a Sink that writes the rendered report to path at RunFinished.
func New(path string) *Sink { return &Sink{path: path} }

// RunStarted implements core.Sink.
func (s *Sink) RunStarted(info core.RunInfo) error {
	s.mu.Lock()
	s.info = info
	s.mu.Unlock()
	return nil
}

// ResultEvent implements core.Sink: it records finished results.
func (s *Sink) ResultEvent(ev core.ResultEvent) error {
	if ev.Kind != core.EventTestFinished || ev.Result == nil {
		return nil
	}
	s.mu.Lock()
	s.results = append(s.results, ev.Result)
	s.mu.Unlock()
	return nil
}

// RunFinished implements core.Sink: it renders and writes the HTML report.
func (s *Sink) RunFinished(sum core.RunSummary) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	out, err := Render(RunData{
		Seed:      s.info.Seed,
		Parallel:  s.info.Parallel,
		StartedAt: sum.StartedAt,
		EndedAt:   sum.EndedAt,
		Results:   s.results,
	})
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(s.path, []byte(out), 0o644)
}
