package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/yugabyte/meko/backend/internal/extensions"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/server"
)

// An extension's state in meko extension list.
const (
	extRunning      = "running"
	extNotRunning   = "not_running"
	extNoAnswer     = "no_answer" // its status didn't answer within extensions.StatusTimeout
	extDamaged      = "damaged"   // registered, but its command is missing and isn't found again
	extNotInstalled = "not_installed"
)

// extensionRow is a row of meko extension list: a known extension, or a registered one, its state,
// and for an installed one what the Extensions page says about its updates.
type extensionRow struct {
	Name      string `json:"name"`
	Title     string `json:"title"`
	Installed bool   `json:"installed"`
	State     string `json:"state"`
	Command   string `json:"command,omitempty"`
	server.ExtensionUpdate
}

// runListExtensions is meko extension list [--json]. It needs no running server: it reads the
// registry, the releases installed and the last check (update.json), and runs every installed
// extension's status command at once, each bounded by extensions.StatusTimeout. A registered
// extension whose command is missing is looked for again (Catalog.Find) without saving what's found,
// and is damaged when it isn't found.
func runListExtensions(l paths.Layout, asJSON bool, stdout, stderr io.Writer) int {
	reg, err := extensions.Load(l.Root)
	if err != nil {
		return fail(stderr, err)
	}
	home, _ := os.UserHomeDir()
	names := extensions.KnownNames()
	for _, n := range reg.Names() {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	rows := make([]extensionRow, 0, len(names))
	// live are the installed extensions whose command is there, with the command found.
	live := &extensions.Registry{Entries: map[string]extensions.Entry{}}
	for _, n := range names {
		row := extensionRow{Name: n, Title: extensionTitle(n), State: extNotInstalled,
			ExtensionUpdate: server.ExtensionUpdate{RolledBack: []string{}}}
		e, ok := reg.Entries[n]
		if ok {
			row.Installed, row.Command = true, e.Command
			if _, serr := os.Stat(e.Command); errors.Is(serr, os.ErrNotExist) {
				e.Command = ""
				if cat, known := extensions.Known[n]; known && home != "" {
					e.Command = cat.Find(n, home, l.Root)
				}
			}
			if e.Command == "" {
				row.State = extDamaged
			} else {
				row.Command = e.Command
				live.Entries[n] = e
				row.ExtensionUpdate = extensionUpdates(l.Root, n, e, extensions.Known[n].AutoUpdate(l.Root, n, e))
			}
		}
		rows = append(rows, row)
	}
	statuses := live.Statuses(context.Background(), l.Root, extensions.StatusTimeout)()
	for i, row := range rows {
		s, ok := statuses[row.Name]
		switch {
		case !ok:
		case s.TimedOut:
			rows[i].State = extNoAnswer
		case s.Running:
			rows[i].State = extRunning
		default:
			rows[i].State = extNotRunning
		}
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			return fail(stderr, err)
		}
		return exitOK
	}
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "EXTENSION\tSTATE\tVERSION\tUPDATES")
	for _, row := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", row.Title, row.stateText(), row.Version, row.updatesText())
	}
	_ = tw.Flush()
	// A row with nothing in its last columns would end in their padding.
	for line := range strings.Lines(b.String()) {
		fmt.Fprintln(stdout, strings.TrimRight(line, " \n"))
	}
	return exitOK
}

// stateText is the row's STATE.
func (r extensionRow) stateText() string {
	switch r.State {
	case extNotRunning:
		return "not running"
	case extNoAnswer:
		return fmt.Sprintf("didn't answer in %s", extensions.StatusTimeout)
	case extDamaged:
		return fmt.Sprintf("damaged (%s is missing)", r.Command)
	case extNotInstalled:
		return "not installed"
	}
	return r.State
}

// updatesText is the row's UPDATES: whether Meko installs them on its own and a newer release, why
// Meko can't update it, or how to forget a damaged one.
func (r extensionRow) updatesText() string {
	switch {
	case r.State == extDamaged:
		return "meko extension delete " + r.Name + " forgets it"
	case !r.Installed:
		return ""
	case !r.Updatable:
		return r.NotUpdatable
	}
	text := "automatic"
	if !r.AutoUpdate {
		text = "by hand (meko extension update " + r.Name + ")"
	}
	if r.UpdateAvailable {
		text += "; " + r.Latest + " available"
	}
	if r.Pending != nil {
		text += "; " + r.Pending.To + " on probation"
	}
	return text
}
