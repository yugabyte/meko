package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/yugabyte/meko/backend/internal/extensions"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/update"
)

// extDeleteOptions are extension delete's: purge also deletes the extension's data, and force
// doesn't ask.
type extDeleteOptions struct{ purge, force bool }

// runDeleteExtension is meko extension delete <name> [--purge] [--force]. It runs the extension's
// own uninstall (Catalog.RemoveArgs: it stops and removes its services, so AgentK's sessions end),
// removes the release Meko installed and the command Meko linked to it, and unregisters it. Its data
// in its folder stays; with purge, its uninstall is UninstallArgs, which deletes its data, and the
// folder goes too. It asks first unless force. A damaged extension (its command is missing) has no
// uninstall to run, and is only removed and unregistered. It needs no running server, and holds the
// update lock, so no install or update runs meanwhile.
func runDeleteExtension(l paths.Layout, name string, o extDeleteOptions, in io.Reader, stdout, stderr io.Writer) int {
	cat, known := extensions.Known[name]
	exts, err := findExtensionData(l)
	if err != nil {
		return fail(stderr, err)
	}
	var ext extensionData
	for _, e := range exts {
		if e.name == name {
			ext = e
		}
	}
	if !known && !ext.registered {
		fmt.Fprintf(stderr, "meko extension delete: no extension called %q: meko knows %s\n", name,
			strings.Join(extensions.KnownNames(), ", "))
		return exitUsage
	}
	title := extensionTitle(name)
	app := cat.App(l.Root, name)
	hasApp := false
	if cat.Managed {
		_, serr := os.Stat(app.Root)
		hasApp = serr == nil
	}
	// Not registered and no release of Meko's left: there's nothing to remove but its data.
	if !ext.registered && !hasApp && (!o.purge || ext.dir == "") {
		if ext.dir != "" {
			return fail(stderr, fmt.Errorf("%s isn't installed; its data stays in %s (meko extension delete %s "+
				"--purge deletes it)", title, ext.dir, name))
		}
		return fail(stderr, fmt.Errorf("%s isn't installed", title))
	}
	if damaged(l, name, ext) {
		fmt.Fprintf(stdout, "%s is damaged (%s is missing), so its own uninstall can't run and any service it "+
			"installed stays: deleting it removes the releases Meko installed and forgets it\n", title, ext.command)
	} else {
		fmt.Fprintf(stdout, "Deleting %s stops it%s and removes its services, the releases Meko installed and its "+
			"command\n", title, sessionsEnd(name))
	}
	if ext.dir != "" {
		size := humanSize(paths.DirSize(ext.dir))
		if o.purge {
			fmt.Fprintf(stdout, "--purge also deletes its data in %s (%s)%s\n", ext.dir, size, purgedWork(name))
		} else {
			fmt.Fprintf(stdout, "Its data in %s (%s) stays: --purge deletes it too\n", ext.dir, size)
		}
	}
	if !o.force && !confirm(newAnswerReader(in), stdout, "Delete it? [y/N] ") {
		fmt.Fprintln(stderr, "Nothing was removed")
		return exitError
	}
	lock := update.NewLock(l.Root)
	if !lock.TryLock() {
		return fail(stderr, update.ErrLocked)
	}
	defer lock.Unlock()
	if err := uninstallExtensions(l, []extensionData{ext}, o.purge, stdout, stderr); err != nil {
		return fail(stderr, err)
	}
	if cat.Managed {
		// A damaged extension's uninstall didn't run, so the link Meko made is removed here.
		if home, herr := os.UserHomeDir(); herr == nil {
			_ = app.UnlinkCommand(home)
		}
	}
	// Only a known extension's folders: a registered name Meko doesn't know is only forgotten.
	removed := ""
	switch {
	case known && o.purge:
		removed = extensions.Dir(l.Root, name)
	case cat.Managed:
		removed = app.Root
	}
	if removed != "" {
		// G703: a known extension's folder (extensions.Known), never a path from the command line.
		if err := os.RemoveAll(removed); err != nil { //nolint:gosec
			return fail(stderr, err)
		}
	}
	if ext.registered {
		reg, err := extensions.Load(l.Root)
		if err != nil {
			return fail(stderr, err)
		}
		delete(reg.Entries, name)
		if err := reg.Save(); err != nil {
			return fail(stderr, err)
		}
	}
	switch {
	case o.purge || ext.dir == "":
		fmt.Fprintf(stdout, "Deleted %s\n", title)
	default:
		fmt.Fprintf(stdout, "Deleted %s. Its data stays in %s: meko extension delete %s --purge deletes it\n",
			title, ext.dir, name)
	}
	return exitOK
}

// damaged says a registered extension's command is missing and isn't found again.
func damaged(l paths.Layout, name string, ext extensionData) bool {
	if !ext.registered {
		return false
	}
	if _, err := os.Stat(ext.command); err == nil {
		return false
	}
	cat, ok := extensions.Known[name]
	home, err := os.UserHomeDir()
	return !ok || err != nil || cat.Find(name, home, l.Root) == ""
}

// sessionsEnd is what else stops with an extension: AgentK's sessions.
func sessionsEnd(name string) string {
	if name == "agentk" {
		return " (every session it runs ends)"
	}
	return ""
}

// purgedWork is what of an extension's data --purge deletes that people would miss.
func purgedWork(name string) string {
	if name == "agentk" {
		return ": AgentK's sessions and worktrees, with any uncommitted work in them"
	}
	return ""
}
