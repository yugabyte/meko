package junit_test

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yugabyte/yb-amp/test/system/framework/core"
	"github.com/yugabyte/yb-amp/test/system/framework/report/junit"
)

type tsuites struct {
	XMLName  xml.Name `xml:"testsuites"`
	Tests    int      `xml:"tests,attr"`
	Failures int      `xml:"failures,attr"`
	Errors   int      `xml:"errors,attr"`
	Skipped  int      `xml:"skipped,attr"`
	Suites   []tsuite `xml:"testsuite"`
}

type tsuite struct {
	Name  string  `xml:"name,attr"`
	Cases []tcase `xml:"testcase"`
}

type tcase struct {
	Name    string   `xml:"name,attr"`
	Failure *tdetail `xml:"failure"`
	Error   *tdetail `xml:"error"`
	Skipped *tskip   `xml:"skipped"`
}

type tdetail struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
}

type tskip struct {
	Message string `xml:"message,attr"`
}

func TestJUnitSinkMapsStatuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "report.xml")
	s := junit.New(path)
	_ = s.RunStarted(core.RunInfo{})

	results := []*core.Result{
		{TestName: "s/pass", Suite: "s", Status: core.StatusPassed, Duration: time.Millisecond},
		{TestName: "s/fail", Suite: "s", Status: core.StatusFailed, ErrorSummary: "assert", Duration: time.Millisecond},
		{TestName: "s/broken", Suite: "s", Status: core.StatusBroken, ErrorSummary: "panic", Duration: time.Millisecond},
		{TestName: "s/env", Suite: "s", Status: core.StatusEnvFailed, ErrorSummary: "no env", Duration: time.Millisecond},
		{TestName: "s/timeout", Suite: "s", Status: core.StatusTimedOut, ErrorSummary: "deadline", Duration: time.Millisecond},
		{TestName: "s/skip", Suite: "s", Status: core.StatusSkipped, SkipReason: "version", Duration: time.Millisecond},
	}
	for _, r := range results {
		_ = s.ResultEvent(core.ResultEvent{Kind: core.EventTestFinished, Result: r})
	}
	if err := s.RunFinished(core.RunSummary{}); err != nil {
		t.Fatalf("RunFinished: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read xml: %v", err)
	}
	var doc tsuites
	if err := xml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse xml: %v", err)
	}

	if doc.Tests != 6 || doc.Failures != 1 || doc.Errors != 3 || doc.Skipped != 1 {
		t.Fatalf("aggregate counts wrong: %+v", doc)
	}
	if len(doc.Suites) != 1 {
		t.Fatalf("suites = %d, want 1", len(doc.Suites))
	}

	byName := map[string]tcase{}
	for _, c := range doc.Suites[0].Cases {
		byName[c.Name] = c
	}
	if c := byName["s/fail"]; c.Failure == nil || c.Failure.Type != "failed" {
		t.Fatalf("fail case not mapped to <failure type=failed>: %+v", c)
	}
	for _, name := range []string{"s/broken", "s/env", "s/timeout"} {
		if c := byName[name]; c.Error == nil {
			t.Fatalf("%s not mapped to <error>", name)
		}
	}
	if byName["s/broken"].Error.Type != "broken" || byName["s/env"].Error.Type != "env_failed" || byName["s/timeout"].Error.Type != "timed_out" {
		t.Fatalf("error types not carried through: %+v", byName)
	}
	if c := byName["s/skip"]; c.Skipped == nil || c.Skipped.Message != "version" {
		t.Fatalf("skip case not mapped: %+v", c)
	}
	if c := byName["s/pass"]; c.Failure != nil || c.Error != nil || c.Skipped != nil {
		t.Fatalf("passed case should have no child element: %+v", c)
	}
}
