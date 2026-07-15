package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/yb-amp/test/system/framework/core"
	"github.com/yugabyte/yb-amp/test/system/framework/report/jsonfile"
)

func writeFixtureRunDir(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	sink := jsonfile.New(base)
	start := time.Now()
	if err := sink.RunStarted(core.RunInfo{Seed: 77, Parallel: 4, StartedAt: start}); err != nil {
		t.Fatalf("RunStarted: %v", err)
	}
	results := []*core.Result{
		{TestName: "suite/one", Suite: "suite", Status: core.StatusPassed, Duration: 2 * time.Second, Seed: 1},
		{TestName: "suite/two", Suite: "suite", Status: core.StatusFailed, ErrorSummary: "boom", Duration: 3 * time.Second, Seed: 2},
	}
	for _, r := range results {
		if err := sink.ResultEvent(core.ResultEvent{Kind: core.EventTestFinished, Result: r}); err != nil {
			t.Fatalf("ResultEvent: %v", err)
		}
	}
	if err := sink.RunFinished(core.RunSummary{
		Total: 2, Counts: map[core.Status]int{core.StatusPassed: 1, core.StatusFailed: 1},
		StartedAt: start, EndedAt: start.Add(5 * time.Second),
	}); err != nil {
		t.Fatalf("RunFinished: %v", err)
	}
	return sink.Dir()
}

// TestReportCmdFromRunDir: `ampqa report --from <run-out-dir> -o report.html`
// must render every test in a jsonfile sink's output directory.
func TestReportCmdFromRunDir(t *testing.T) {
	dir := writeFixtureRunDir(t)
	outPath := filepath.Join(t.TempDir(), "report.html")

	root := buildRoot()
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetArgs([]string{"report", "--from", dir, "-o", outPath, "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("report: %v", err)
	}

	var env struct {
		Data struct {
			Total  int            `json:"total"`
			Counts map[string]int `json:"counts"`
			Out    string         `json:"out"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, stdout.String())
	}
	if env.Data.Total != 2 {
		t.Errorf("total = %d, want 2", env.Data.Total)
	}
	if env.Data.Counts["passed"] != 1 || env.Data.Counts["failed"] != 1 {
		t.Errorf("counts = %+v, want passed=1 failed=1", env.Data.Counts)
	}

	html, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	out := string(html)
	if !strings.HasPrefix(out, "<!doctype html>") {
		t.Fatal("report is not an HTML document")
	}
	if !strings.Contains(out, "suite/one") || !strings.Contains(out, "suite/two") {
		t.Fatal("report missing test rows from run dir")
	}
	if !strings.Contains(out, "boom") {
		t.Fatal("report missing error summary from run dir")
	}
}

// TestReportCmdFromResultsFile: --from also accepts a plain JSON array of
// results (not just a jsonfile run-out-dir).
func TestReportCmdFromResultsFile(t *testing.T) {
	results := []*core.Result{
		{TestName: "adhoc/case", Suite: "adhoc", Status: core.StatusSkipped, SkipReason: "manual"},
	}
	raw, err := json.Marshal(results)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	resultsPath := filepath.Join(t.TempDir(), "results.json")
	if err := os.WriteFile(resultsPath, raw, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	outPath := filepath.Join(t.TempDir(), "report.html")

	root := buildRoot()
	root.SetArgs([]string{"report", "--from", resultsPath, "-o", outPath})
	if err := root.Execute(); err != nil {
		t.Fatalf("report: %v", err)
	}
	out, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if !strings.Contains(string(out), "adhoc/case") {
		t.Fatal("report missing test from results.json fixture")
	}
}

// TestReportCmdRequiresFromAndOut checks the usage-error exit path (code 2)
// when required flags are missing.
func TestReportCmdRequiresFromAndOut(t *testing.T) {
	root := buildRoot()
	root.SetArgs([]string{"report", "-o", filepath.Join(t.TempDir(), "report.html")})
	root.SetOut(&bytes.Buffer{})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected an error when --from is missing")
	}
	var ee *exitError
	if !asExitError(err, &ee) || ee.code != 2 {
		t.Fatalf("error = %v, want usage exitError(2)", err)
	}
}

// TestReportCmdMissingFromPath checks the not-found path returns exit code 1.
func TestReportCmdMissingFromPath(t *testing.T) {
	root := buildRoot()
	root.SetArgs([]string{"report", "--from", filepath.Join(t.TempDir(), "does-not-exist"), "-o", filepath.Join(t.TempDir(), "report.html")})
	root.SetOut(&bytes.Buffer{})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected an error for a missing --from path")
	}
	var ee *exitError
	if !asExitError(err, &ee) || ee.code != 1 {
		t.Fatalf("error = %v, want exitError(1)", err)
	}
}

func asExitError(err error, target **exitError) bool {
	if ee, ok := err.(*exitError); ok {
		*target = ee
		return true
	}
	return false
}
