package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"unicode/utf8"

	"github.com/yugabyte/meko/src/meko/internal/spin"
)

// The summary install, start, stop, restart and status print: a row per step, marked done (✓), a
// warning (!) or failed (✗); a box of links; and the commands to run next. Extensions print their
// start and stop rows in the same format when Meko runs them with extensions.SummaryEnv set
// (docs/install.md, "The summary rows"). The marks and the row format are package spin's, whose
// steps show the same rows with rotating dots while they run.

type stepKind = spin.Kind

const (
	stepOK   = spin.OK
	stepWarn = spin.Warn
	stepFail = spin.Fail
)

// report writes the summary to w, in color when w is a terminal and NO_COLOR is unset.
type report struct {
	w     io.Writer
	color bool
}

func newReport(w io.Writer) report {
	return report{w: w, color: spin.Color(w)}
}

func (r report) mark(k stepKind) string { return spin.Mark(k, r.color) }

// step prints one row: "  ✓ Server          running".
func (r report) step(k stepKind, label, text string) { spin.Row(r.w, k, r.color, label, text) }

// headline prints a line marked as a step is, unindented: status's first line.
func (r report) headline(k stepKind, text string) {
	fmt.Fprintf(r.w, "%s %s\n", r.mark(k), text)
}

// box prints a title and label/value rows in a box as wide as its widest line.
func (r report) box(title string, rows [][2]string) {
	labelW := 0
	for _, row := range rows {
		labelW = max(labelW, utf8.RuneCountInString(row[0]))
	}
	lines := make([]string, len(rows))
	inner := utf8.RuneCountInString(title)
	for i, row := range rows {
		lines[i] = row[0] + strings.Repeat(" ", labelW-utf8.RuneCountInString(row[0])+3) + row[1]
		inner = max(inner, utf8.RuneCountInString(lines[i]))
	}
	pad := func(s string) string { return s + strings.Repeat(" ", inner-utf8.RuneCountInString(s)) }
	rule := strings.Repeat("─", inner+2)
	fmt.Fprintf(r.w, "\n┌%s┐\n│ %s │\n├%s┤\n", rule, pad(title), rule)
	for _, l := range lines {
		fmt.Fprintf(r.w, "│ %s │\n", pad(l))
	}
	fmt.Fprintf(r.w, "└%s┘\n", rule)
}

// next prints the commands to run next, each with what it does.
func (r report) next(cmds [][2]string) {
	fmt.Fprintf(r.w, "\nNext:\n")
	tw := tabwriter.NewWriter(r.w, 0, 0, 4, ' ', 0)
	for _, c := range cmds {
		fmt.Fprintf(tw, "  %s\t%s\n", c[0], c[1])
	}
	_ = tw.Flush()
}
