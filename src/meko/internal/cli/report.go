package cli

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/yugabyte/meko/src/meko/internal/spin"
)

// The summary install, start, stop, restart and status print: a row per step, marked done (✓), a
// warning (!) or failed (✗); a ● headline and the links, as plain aligned rows; and the commands to
// run next. Extensions print their
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

// running prints the headline of a running Meko, after a blank line: "  ● Meko 1.2 is running · pid
// 7, up 2h", its text bold and its note dim.
func (r report) running(text, note string) {
	fmt.Fprintf(r.w, "\n  %s %s\n", spin.Green("●", r.color), spin.Note(spin.Bold(text, r.color), note, r.color))
}

// linkRow is a line of links: where people or agents reach Meko. url is "" for a row that has only
// a note ("Cloud    not linked"), and label is "" for a second URL under the row above it (the
// REST API's under Agents).
type linkRow struct{ label, url, note string }

// links prints the link rows, after a blank line, their labels dim and aligned, and each URL
// followed only by its dim note after " · ", so it copies cleanly: blue, underlined and an OSC 8
// link on a terminal.
func (r report) links(rows []linkRow) {
	labelW := 0
	for _, row := range rows {
		labelW = max(labelW, utf8.RuneCountInString(row.label))
	}
	fmt.Fprintln(r.w)
	for _, row := range rows {
		label := spin.Dim(row.label, r.color) + strings.Repeat(" ", labelW-utf8.RuneCountInString(row.label))
		if row.url == "" {
			fmt.Fprintf(r.w, "  %s  %s\n", label, row.note)
			continue
		}
		fmt.Fprintf(r.w, "  %s  %s\n", label, spin.Note(spin.Link(row.url, r.color), row.note, r.color))
	}
}

// next prints the commands to run next, each with what it does: the first as the call to action
// and the rest blue on a terminal, and what follows a " · " in what it does dim.
func (r report) next(cmds [][2]string) { r.nextUnder("Next:", cmds) }

// nextInNewTerminal is next for an install that just put meko on the PATH in the shell's profile:
// this terminal keeps its PATH, so the commands run in a new one.
const nextInNewTerminal = "Next, in a new terminal:"

// nextUnder is next under another title.
func (r report) nextUnder(title string, cmds [][2]string) {
	fmt.Fprintf(r.w, "\n%s\n", title)
	cmdW := 0
	for _, c := range cmds {
		cmdW = max(cmdW, utf8.RuneCountInString(c[0]))
	}
	for i, c := range cmds {
		pad := strings.Repeat(" ", cmdW-utf8.RuneCountInString(c[0])+4)
		what, note, _ := strings.Cut(c[1], " · ")
		what = spin.Note(what, note, r.color)
		switch {
		case i == 0 && r.color:
			// Its blue starts a column early, so its text lines up with the commands below it.
			fmt.Fprintf(r.w, " %s%s%s\n", spin.CTA(c[0], true), pad[1:], what)
		default:
			fmt.Fprintf(r.w, "  %s%s%s\n", spin.Blue(c[0], r.color), pad, what)
		}
	}
}
