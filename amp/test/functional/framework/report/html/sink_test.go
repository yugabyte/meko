package html_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
	rhtml "github.com/yugabyte/amp/test/functional/framework/report/html"
)

func TestSinkWritesReportAtRunFinished(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "report.html")
	s := rhtml.New(path)

	start := time.Now()
	if err := s.RunStarted(core.RunInfo{Seed: 5, Parallel: 3, StartedAt: start}); err != nil {
		t.Fatalf("RunStarted: %v", err)
	}
	for _, r := range fixtureResults() {
		if err := s.ResultEvent(core.ResultEvent{Kind: core.EventTestFinished, Result: r}); err != nil {
			t.Fatalf("ResultEvent: %v", err)
		}
	}
	// Non-finished events must be ignored.
	if err := s.ResultEvent(core.ResultEvent{Kind: core.EventTestStarted, TestName: "suite/pass_test"}); err != nil {
		t.Fatalf("ResultEvent (started): %v", err)
	}
	if err := s.RunFinished(core.RunSummary{Total: 3, StartedAt: start, EndedAt: start.Add(10 * time.Second)}); err != nil {
		t.Fatalf("RunFinished: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	out := string(data)
	if !strings.HasPrefix(out, "<!doctype html>") {
		t.Fatal("report file is not an HTML document")
	}
	if !strings.Contains(out, "suite/pass_test") || !strings.Contains(out, "suite/fail_test") {
		t.Fatal("report file missing test rows")
	}
	if !strings.Contains(out, "seed 5") {
		t.Error("report file missing run seed")
	}
}
