package report_test

import (
	"sync"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/report"
)

type recordingSink struct {
	mu       sync.Mutex
	runs     int
	results  int
	finished int
}

func (s *recordingSink) RunStarted(core.RunInfo) error {
	s.mu.Lock()
	s.runs++
	s.mu.Unlock()
	return nil
}

func (s *recordingSink) ResultEvent(core.ResultEvent) error {
	s.mu.Lock()
	s.results++
	s.mu.Unlock()
	return nil
}

func (s *recordingSink) RunFinished(core.RunSummary) error {
	s.mu.Lock()
	s.finished++
	s.mu.Unlock()
	return nil
}

func (s *recordingSink) counts() (int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs, s.results, s.finished
}

func TestReporterFanOut(t *testing.T) {
	a := &recordingSink{}
	b := &recordingSink{}
	r := report.NewReporter(a, b)

	_ = r.RunStarted(core.RunInfo{Total: 5})
	const nEvents = 50
	for i := 0; i < nEvents; i++ {
		_ = r.ResultEvent(core.ResultEvent{Kind: core.EventTestStarted, TestName: "x"})
	}
	_ = r.RunFinished(core.RunSummary{})
	r.Close()

	for _, s := range []*recordingSink{a, b} {
		runs, results, finished := s.counts()
		if runs != 1 || results != nEvents || finished != 1 {
			t.Errorf("sink got runs=%d results=%d finished=%d, want 1/%d/1", runs, results, finished, nEvents)
		}
	}
	if r.Dropped() != 0 {
		t.Errorf("no drops expected, got %d", r.Dropped())
	}
}

// blockingSink stalls in ResultEvent until gate is closed, so the reporter's
// bounded queue overflows and drops.
type blockingSink struct {
	gate chan struct{}
}

func (s *blockingSink) RunStarted(core.RunInfo) error { return nil }
func (s *blockingSink) ResultEvent(core.ResultEvent) error {
	<-s.gate
	return nil
}
func (s *blockingSink) RunFinished(core.RunSummary) error { return nil }

func TestReporterOverflowDrops(t *testing.T) {
	bs := &blockingSink{gate: make(chan struct{})}
	r := report.NewReporterWithOptions([]report.SinkOption{{Sink: bs}}, 2)

	for i := 0; i < 200; i++ {
		_ = r.ResultEvent(core.ResultEvent{Kind: core.EventTestStarted})
	}
	if r.Dropped() == 0 {
		t.Errorf("expected drops on overflow of a blocked sink, got 0")
	}
	close(bs.gate) // release so Close can drain and finish
	r.Close()
}

func TestReporterCriticalSinkBlocksNoDrop(t *testing.T) {
	a := &recordingSink{}
	r := report.NewReporterWithOptions([]report.SinkOption{{Sink: a, Critical: true}}, 4)
	const n = 100
	for i := 0; i < n; i++ {
		_ = r.ResultEvent(core.ResultEvent{Kind: core.EventTestStarted})
	}
	r.Close()
	if r.Dropped() != 0 {
		t.Errorf("critical sink must not drop, got %d", r.Dropped())
	}
	if _, results, _ := a.counts(); results != n {
		t.Errorf("critical sink got %d results, want %d", results, n)
	}
}
