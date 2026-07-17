// Package junit is a Sink that writes standard JUnit XML at RunFinished. Status
// taxonomy maps as: failed→<failure>, broken/env_failed/timed_out→<error> (with
// type=status), skipped→<skipped>, passed→no child.
package junit

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"

	"github.com/yugabyte/yb-amp/test/functional/framework/core"
)

// Sink accumulates results and writes the XML file at RunFinished.
type Sink struct {
	path string

	mu      sync.Mutex
	results []*core.Result
}

// New returns a Sink writing to path.
func New(path string) *Sink { return &Sink{path: path} }

// RunStarted implements core.Sink.
func (s *Sink) RunStarted(core.RunInfo) error { return nil }

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

// RunFinished implements core.Sink: it renders and writes the JUnit XML.
func (s *Sink) RunFinished(core.RunSummary) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc := build(s.results)
	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	body := append([]byte(xml.Header), out...)
	body = append(body, '\n')
	return os.WriteFile(s.path, body, 0o644)
}

type testsuites struct {
	XMLName  xml.Name    `xml:"testsuites"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Errors   int         `xml:"errors,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Time     float64     `xml:"time,attr"`
	Suites   []testsuite `xml:"testsuite"`
}

type testsuite struct {
	Name     string     `xml:"name,attr"`
	Tests    int        `xml:"tests,attr"`
	Failures int        `xml:"failures,attr"`
	Errors   int        `xml:"errors,attr"`
	Skipped  int        `xml:"skipped,attr"`
	Time     float64    `xml:"time,attr"`
	Cases    []testcase `xml:"testcase"`
}

type testcase struct {
	Name       string      `xml:"name,attr"`
	Classname  string      `xml:"classname,attr"`
	Time       float64     `xml:"time,attr"`
	Properties *properties `xml:"properties,omitempty"`
	Failure    *detail     `xml:"failure,omitempty"`
	Error      *detail     `xml:"error,omitempty"`
	Skipped    *skipmark   `xml:"skipped,omitempty"`
}

// properties carries a testcase's perf datapoints as standard JUnit
// <property> elements: name="perf.<datapoint>" value="<value> <unit>".
type properties struct {
	Property []property `xml:"property"`
}

type property struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type detail struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
}

type skipmark struct {
	Message string `xml:"message,attr,omitempty"`
}

func build(results []*core.Result) testsuites {
	bySuite := map[string][]*core.Result{}
	var order []string
	for _, r := range results {
		if _, ok := bySuite[r.Suite]; !ok {
			order = append(order, r.Suite)
		}
		bySuite[r.Suite] = append(bySuite[r.Suite], r)
	}
	sort.Strings(order)

	var doc testsuites
	for _, suite := range order {
		ts := testsuite{Name: suite}
		for _, r := range bySuite[suite] {
			tc := testcase{
				Name:      r.TestName,
				Classname: r.Suite,
				Time:      r.Duration.Seconds(),
			}
			if props := perfProperties(r); props != nil {
				tc.Properties = props
			}
			switch r.Status {
			case core.StatusFailed:
				tc.Failure = &detail{Message: r.ErrorSummary, Type: string(core.StatusFailed)}
				ts.Failures++
			case core.StatusBroken, core.StatusEnvFailed, core.StatusTimedOut:
				tc.Error = &detail{Message: r.ErrorSummary, Type: string(r.Status)}
				ts.Errors++
			case core.StatusSkipped:
				tc.Skipped = &skipmark{Message: r.SkipReason}
				ts.Skipped++
			}
			ts.Cases = append(ts.Cases, tc)
			ts.Tests++
			ts.Time += tc.Time
		}
		doc.Tests += ts.Tests
		doc.Failures += ts.Failures
		doc.Errors += ts.Errors
		doc.Skipped += ts.Skipped
		doc.Time += ts.Time
		doc.Suites = append(doc.Suites, ts)
	}
	return doc
}

// perfProperties renders a result's perf datapoints as JUnit <property>
// elements (perf.<name> → "<value> <unit>", plus a baseline delta line when
// gated). Returns nil when the result has no perf datapoints.
func perfProperties(r *core.Result) *properties {
	if len(r.Perf) == 0 {
		return nil
	}
	props := &properties{}
	for _, dp := range r.Perf {
		val := strconv.FormatFloat(dp.Value, 'g', -1, 64)
		if dp.Unit != "" {
			val += " " + dp.Unit
		}
		if b := dp.Baseline; b != nil {
			val += fmt.Sprintf(" (baseline %s, %+.1f%%, tol %.1f%%%s)",
				strconv.FormatFloat(b.Value, 'g', -1, 64), b.DeltaPct, b.Threshold, regressedSuffix(b.Regressed))
		}
		props.Property = append(props.Property, property{Name: "perf." + dp.Name, Value: val})
	}
	return props
}

func regressedSuffix(regressed bool) string {
	if regressed {
		return ", REGRESSED"
	}
	return ""
}
