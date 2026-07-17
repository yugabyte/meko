// Package stdout is a human-readable live Sink: test start/finish with status
// and duration, indented step lines, and a final summary table. Colors are
// hand-rolled ANSI, enabled only on a TTY with NO_COLOR unset.
package stdout

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/yugabyte/yb-amp/test/functional/framework/core"
)

// Sink writes live progress to an io.Writer.
type Sink struct {
	w     io.Writer
	color bool
}

// New returns a Sink writing to os.Stdout with color auto-detected.
func New() *Sink {
	return NewWriter(os.Stdout, colorEnabled(os.Stdout))
}

// NewWriter returns a Sink writing to w with color forced on or off.
func NewWriter(w io.Writer, color bool) *Sink {
	return &Sink{w: w, color: color}
}

func colorEnabled(f *os.File) bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

const (
	ansiReset  = "\033[0m"
	ansiGreen  = "\033[32m"
	ansiRed    = "\033[31m"
	ansiYellow = "\033[33m"
	ansiGray   = "\033[90m"
	ansiBold   = "\033[1m"
)

func (s *Sink) paint(code, text string) string {
	if !s.color {
		return text
	}
	return code + text + ansiReset
}

func statusColor(st core.Status) string {
	switch st {
	case core.StatusPassed:
		return ansiGreen
	case core.StatusFailed, core.StatusBroken, core.StatusEnvFailed, core.StatusTimedOut:
		return ansiRed
	case core.StatusSkipped:
		return ansiYellow
	default:
		return ansiGray
	}
}

// RunStarted implements core.Sink.
func (s *Sink) RunStarted(info core.RunInfo) error {
	fmt.Fprintf(s.w, "%s %d test(s), parallel=%d, seed=%d\n",
		s.paint(ansiBold, "==> run started:"), info.Total, info.Parallel, info.Seed)
	return nil
}

// ResultEvent implements core.Sink.
func (s *Sink) ResultEvent(ev core.ResultEvent) error {
	switch ev.Kind {
	case core.EventTestStarted:
		fmt.Fprintf(s.w, "%s %s\n", s.paint(ansiGray, "RUN "), ev.TestName)
	case core.EventTestFinished:
		st := ev.Result.Status
		label := strings.ToUpper(string(st))
		line := fmt.Sprintf("%-5s %s (%s)", label, ev.TestName, ev.Result.Duration.Round(1e6))
		fmt.Fprintln(s.w, s.paint(statusColor(st), line))
		if ev.Result.SkipReason != "" {
			fmt.Fprintf(s.w, "      %s\n", s.paint(ansiYellow, "skip: "+ev.Result.SkipReason))
		}
		if ev.Result.ErrorSummary != "" {
			fmt.Fprintf(s.w, "      %s\n", s.paint(ansiRed, ev.Result.ErrorSummary))
		}
		for _, dp := range ev.Result.Perf {
			s.printPerf(dp)
		}
		if n := len(ev.Result.Charts); n > 0 {
			fmt.Fprintf(s.w, "      %s\n", s.paint(ansiGray, fmt.Sprintf("%d chart(s)", n)))
		}
	case core.EventStepFinished:
		indent := strings.Repeat("  ", ev.Step.Depth+2)
		mark := s.paint(statusColor(ev.Step.Status), string(ev.Step.Status))
		fmt.Fprintf(s.w, "%s- %s [%s] (%s)\n", indent, ev.Step.Name, mark, ev.Step.Duration().Round(1e6))
	}
	return nil
}

// printPerf writes one perf datapoint line, appending the baseline comparison
// (delta vs baseline, allowed threshold, and a REGRESSED marker) when the
// datapoint was gated. A regression is painted red, otherwise green.
func (s *Sink) printPerf(dp core.Datapoint) {
	line := fmt.Sprintf("perf: %s %g", dp.Name, dp.Value)
	if dp.Unit != "" {
		line += " " + dp.Unit
	}
	color := ansiGreen
	if b := dp.Baseline; b != nil {
		line += fmt.Sprintf(" (baseline %g, %+.1f%%, tol %.1f%%", b.Value, b.DeltaPct, b.Threshold)
		if b.Regressed {
			line += " REGRESSED)"
			color = ansiRed
		} else {
			line += ")"
		}
	} else {
		line += " (no baseline)"
		color = ansiYellow
	}
	fmt.Fprintf(s.w, "      %s\n", s.paint(color, line))
}

// RunFinished implements core.Sink.
func (s *Sink) RunFinished(sum core.RunSummary) error {
	fmt.Fprintln(s.w, s.paint(ansiBold, "==> summary"))
	for _, st := range core.AllStatuses {
		n := sum.Counts[st]
		if n == 0 {
			continue
		}
		fmt.Fprintf(s.w, "  %-10s %d\n", s.paint(statusColor(st), string(st)), n)
	}
	fmt.Fprintf(s.w, "  %-10s %d (%s)\n", "total", sum.Total, sum.EndedAt.Sub(sum.StartedAt).Round(1e6))
	return nil
}
