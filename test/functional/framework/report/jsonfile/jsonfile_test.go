package jsonfile_test

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/report/jsonfile"
)

func TestJSONFileSinkWritesFiles(t *testing.T) {
	base := t.TempDir()
	s := jsonfile.New(base)

	start := time.Now()
	if err := s.RunStarted(core.RunInfo{Seed: 42, Total: 2, Parallel: 2, StartedAt: start}); err != nil {
		t.Fatalf("RunStarted: %v", err)
	}
	dir := s.Dir()
	if dir == "" {
		t.Fatalf("no run dir resolved")
	}

	r1 := &core.Result{TestName: "suite/one", Suite: "suite", Status: core.StatusPassed, Seed: 7, Duration: 3 * time.Millisecond}
	r1.Steps = []*core.Step{{Name: "step", Status: core.StatusPassed}}
	r2 := &core.Result{TestName: "suite/two", Suite: "suite", Status: core.StatusFailed, ErrorSummary: "boom", Seed: 8, Duration: 5 * time.Millisecond}
	for _, r := range []*core.Result{r1, r2} {
		if err := s.ResultEvent(core.ResultEvent{Kind: core.EventTestFinished, Result: r}); err != nil {
			t.Fatalf("ResultEvent: %v", err)
		}
	}
	if err := s.RunFinished(core.RunSummary{Total: 2, Counts: map[core.Status]int{core.StatusPassed: 1, core.StatusFailed: 1}, StartedAt: start, EndedAt: time.Now()}); err != nil {
		t.Fatalf("RunFinished: %v", err)
	}

	// Per-result files exist with the full result content.
	oneRaw, err := os.ReadFile(filepath.Join(dir, "suite_one.json"))
	if err != nil {
		t.Fatalf("read result file: %v", err)
	}
	var one map[string]any
	if err := json.Unmarshal(oneRaw, &one); err != nil {
		t.Fatalf("parse result file: %v", err)
	}
	if one["TestName"] != "suite/one" || one["Status"] != "passed" {
		t.Fatalf("result file content unexpected: %v", one)
	}
	if _, ok := one["Steps"]; !ok {
		t.Fatalf("result file missing steps")
	}

	// run.json parses and carries the counts.
	runRaw, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	var run map[string]any
	if err := json.Unmarshal(runRaw, &run); err != nil {
		t.Fatalf("parse run.json: %v", err)
	}
	if run["total"].(float64) != 2 {
		t.Fatalf("run.json total = %v, want 2", run["total"])
	}

	// manifest.jsonl has one parseable line per finished test.
	mf, err := os.Open(filepath.Join(dir, "manifest.jsonl"))
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer mf.Close()
	lines := 0
	sc := bufio.NewScanner(mf)
	for sc.Scan() {
		var row map[string]any
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			t.Fatalf("manifest line not JSON: %v", err)
		}
		if row["name"] == "" || row["status"] == "" {
			t.Fatalf("manifest line missing fields: %v", row)
		}
		lines++
	}
	if lines != 2 {
		t.Fatalf("manifest lines = %d, want 2", lines)
	}
}
