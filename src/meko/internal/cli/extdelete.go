package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/yugabyte/meko/src/meko/internal/extensions"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/update"
)

// extDeleteOptions are extension delete's: purge also deletes the extension's data, and force
// doesn't ask.
type extDeleteOptions struct{ purge, force bool }

// runDeleteExtension is meko extension delete <name> [--purge] [--force]. It runs the extension's
// own uninstall (Catalog.RemoveArgs: it stops and removes its services, so AgentK's sessions end),
// removes the releases Meko installed and the command Meko linked to them, and unregisters it. Its
// data in its folder stays; with purge, its uninstall is UninstallArgs, which deletes its data, and
// the folder goes too. It asks first unless force. A damaged extension (its command is missing) has
// no uninstall to run: it's only removed and unregistered, and a purge of it is refused, since a
// service it installed may still use its folder. Releases a live process still runs from (a session
// host whose stop failed) are kept, and so is its registration, with an error. It needs no running
// server. It holds the update lock, so no update or release download runs meanwhile.
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
			return fail(stderr, fmt.Errorf("%s isn't installed; its data stays in %s (to delete it too, install it "+
				"again with meko extension install %s, then run meko extension delete %s --purge)", title, ext.dir,
				name, name))
		}
		return fail(stderr, fmt.Errorf("%s isn't installed", title))
	}
	cmd := deleteCommand(l, name, ext)
	if o.purge && known && cmd == "" && ext.dir != "" {
		return fail(stderr, fmt.Errorf("%s's command isn't on PATH, in ~/.local/bin or in its folder, so its own "+
			"uninstall can't run, and a service it installed may still use %s. Nothing was removed: install it again "+
			"(meko extension install %s) and run this again, or run it without --purge", title, ext.dir, name))
	}
	switch {
	case !known:
		fmt.Fprintf(stdout, "Meko doesn't know how to uninstall %s, so it stays as it is: deleting it only forgets "+
			"it\n", title)
	case cmd == "":
		fmt.Fprintf(stdout, "%s is damaged (%s is missing), so its own uninstall can't run and any service it "+
			"installed stays: deleting it removes the releases Meko installed and forgets it\n", title, ext.command)
	case cat.Release(l.Root, name, cmd) == "":
		fmt.Fprintf(stdout, "Deleting %s stops it%s and removes its services. Its command (%s) isn't a release "+
			"Meko installed, so Meko leaves it\n", title, sessionsEnd(name), cmd)
	default:
		fmt.Fprintf(stdout, "Deleting %s stops it%s and removes its services, the releases Meko installed and its "+
			"command\n", title, sessionsEnd(name))
	}
	if ext.dir != "" && known {
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
	if err := removeExtension(l, ext, o.purge, stdout, stderr); err != nil {
		return fail(stderr, err)
	}
	if o.purge || ext.dir == "" || !known {
		fmt.Fprintf(stdout, "Deleted %s\n", title)
	} else {
		fmt.Fprintf(stdout, "Deleted %s. Its data stays in %s\n", title, ext.dir)
	}
	return exitOK
}

// removeExtension is extension delete's work, and uninstall's for each extension, with the update
// lock held: the extension's own uninstall (RemoveArgs, or UninstallArgs with purge), then the
// releases Meko installed and the command it linked, its folder too with purge, and its
// registration. Releases a live process still runs from are kept, with its registration, and that's
// an error.
func removeExtension(l paths.Layout, ext extensionData, purge bool, stdout, stderr io.Writer) error {
	name := ext.name
	cat, known := extensions.Known[name]
	title := extensionTitle(name)
	app := cat.App(l.Root, name)
	if err := uninstallExtensions(l, []extensionData{ext}, purge, stdout, stderr); err != nil {
		return err
	}
	// A version a live process still runs from (AgentK's session host, when its stop failed) keeps
	// its release, which removing the app folder would pull out from under it.
	if live := app.Live(); cat.Managed && len(live) > 0 {
		return fmt.Errorf("%s's uninstall ran, but something still runs from %s %s, so its releases "+
			"and registration were kept: stop it, then run this again", title, title, strings.Join(live, ", "))
	}
	if cat.Managed {
		// A damaged extension's uninstall didn't run, so the links an older Meko made are removed here.
		if home, herr := os.UserHomeDir(); herr == nil {
			_ = cat.UnlinkCommands(app, home)
		}
	}
	// Only a known extension's folders: a registered name Meko doesn't know is only forgotten.
	removed := ""
	switch {
	case known && purge:
		removed = extensions.Dir(l.Root, name)
	case cat.Managed:
		removed = app.Root
	}
	if removed != "" {
		// G703: a known extension's folder (extensions.Known), never a path from the command line.
		if err := os.RemoveAll(removed); err != nil { //nolint:gosec
			return err
		}
	}
	if ext.registered {
		reg, err := extensions.Load(l.Root)
		if err != nil {
			return err
		}
		delete(reg.Entries, name)
		if err := reg.Save(); err != nil {
			return err
		}
	}
	return nil
}

// deleteCommand is the command extension delete runs the uninstall of, as uninstallExtensions finds
// it: the registered one, else Catalog.Find's. "" means its uninstall can't run.
func deleteCommand(l paths.Layout, name string, ext extensionData) string {
	if _, err := os.Stat(ext.command); ext.command != "" && err == nil {
		return ext.command
	}
	cat, ok := extensions.Known[name]
	home, err := os.UserHomeDir()
	if !ok || err != nil {
		return ""
	}
	return cat.Find(name, home, l.Root)
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
