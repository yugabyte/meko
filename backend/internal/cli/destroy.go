package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yugabyte/meko/backend/internal/cloudlink"
	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/extensions"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/spin"
)

// destroyOptions are destroy's flags: force asks nothing, withExtensions removes the extensions'
// data too (each extension's own uninstall).
type destroyOptions struct {
	force, withExtensions bool
}

// extensionData is one extension with data in the data folder: registered in extensions.json, or a
// known one whose folder (extensions.Dir) is there.
type extensionData struct {
	name, dir, command string
	registered         bool
}

// runDestroy stops the server and removes the data folder, after asking (unless force).
func runDestroy(l paths.Layout, o destroyOptions, in io.Reader, stdout, stderr io.Writer) int {
	code, _ := destroy(l, o, in, stdout, stderr)
	return code
}

// destroy stops the server and removes the data folder, after asking (unless force). It also
// reports whether it kept the extensions' data.
//
// Only a folder that looks like Meko's is removed. Without extension data the folder is renamed
// away while the lock is held, so a start at the same moment finds either all of it or none of it,
// and then removed. With extension data it asks whether to remove that too (or follows
// o.withExtensions; force alone keeps it): removing runs each extension's own uninstall first, then
// removes the folder as above; keeping moves everything else aside under the lock instead
// (removeKeepingExtensions), so an extension running from its folder (AgentK's sessions) never
// loses it. A symlinked data folder loses the folder it points to and then the link. A missing or
// empty folder is nothing to remove, and is left as it is (a link to it too).
func destroy(l paths.Layout, o destroyOptions, in io.Reader, stdout, stderr io.Writer) (int, bool) {
	link := ""
	fi, err := os.Lstat(l.Root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		fmt.Fprintf(stdout, "Nothing to remove: there's no data folder at %s\n", l.Root)
		return exitOK, false
	case err != nil:
		return fail(stderr, err), false
	case fi.Mode()&fs.ModeSymlink != 0:
		target, lerr := filepath.EvalSymlinks(l.Root)
		if lerr != nil {
			return fail(stderr, fmt.Errorf("the data folder %s is a symlink that can't be followed: %w", l.Root, lerr)), false
		}
		link, l = l.Root, paths.New(target)
	}
	// Resolving the default folder on an NFS home makes /var/tmp/<user>/meko, so an empty one is
	// nothing to remove rather than a folder that isn't Meko's.
	if entries, rerr := os.ReadDir(l.Root); rerr == nil && len(entries) == 0 {
		fmt.Fprintf(stdout, "Nothing to remove: the data folder %s is empty\n", l.Root)
		return exitOK, false
	}
	exts, err := findExtensionData(l)
	if err != nil {
		return fail(stderr, err), false
	}
	if !paths.LooksLikeDataDir(l.Root) {
		if len(exts) == 0 {
			return fail(stderr, fmt.Errorf("%s doesn't look like Meko's data folder (it has no meko.db, which "+
				"only a server makes), so it wasn't removed", l.Root)), false
		}
		return destroyOnlyExtensions(l, exts, o, in, stdout, stderr), !o.withExtensions
	}
	up, err := running(l.Lock)
	if err != nil {
		return fail(stderr, err), false
	}
	fmt.Fprintf(stdout, "This removes Meko's data in %s (%s) for good: every datapack, memory and "+
		"conversation, the config, the client token and the logs\n", l.Root,
		humanSize(paths.DirSize(l.Root, l.Extensions)))
	listExtensionData(exts, o, stdout)
	if up {
		fmt.Fprintln(stdout, "Meko is running, so it's stopped first")
	}
	answers := newAnswerReader(in)
	if !o.force && !confirm(answers, stdout, "Destroy it? [y/N] ") {
		fmt.Fprintln(stderr, "Nothing was removed")
		return exitError, false
	}
	removeExts := o.withExtensions
	if len(exts) > 0 && !removeExts && !o.force {
		removeExts = confirm(answers, stdout, "Also uninstall the extensions and delete their data? [y/N] ")
	}
	keep := len(exts) > 0 && !removeExts
	// The client token and the Meko Cloud link's key may be outside the folder (the login Keychain):
	// found now, deleted after.
	cfg, cerr := config.Load(l.Config, os.Getenv, config.Overrides{})
	if cerr != nil {
		cfg = config.Default()
	}
	ct, cterr := clientTokens(l, cfg)

	nothing := "Nothing was removed"
	if removeExts {
		if err = uninstallExtensions(l, exts, true, stdout, stderr); err != nil {
			return fail(stderr, err), false
		}
		nothing = "The extensions were uninstalled and their data deleted already, but nothing of Meko's was removed"
	}
	// Under its service, a server that only gets a signal is started again: stop the service first.
	m := serviceOf(l)
	if m != nil {
		if err = m.Stop(); err != nil {
			return fail(stderr, fmt.Errorf("%w. %s", err, nothing)), false
		}
	}
	msg, err := stopServer(l)
	if err != nil {
		return fail(stderr, fmt.Errorf("%w. %s", err, nothing)), false
	}
	if up {
		fmt.Fprintln(stdout, msg)
	}
	if keep {
		err = removeKeepingExtensions(l)
	} else {
		err = removeDataDir(l)
	}
	// Once the folder has been moved away, the link points at nothing, even if removing it failed.
	if _, serr := os.Lstat(l.Root); link != "" && errors.Is(serr, fs.ErrNotExist) {
		if lerr := paths.RemoveIfExists(link); lerr != nil {
			err = errors.Join(err, fmt.Errorf("the symlink %s to it is left: %w", link, lerr))
		}
	}
	if err != nil {
		return fail(stderr, err), false
	}
	if cterr == nil {
		if err = ct.Delete(); err != nil {
			return fail(stderr, fmt.Errorf("removed %s, but not its client token: %w", l.Root, err)), false
		}
		if err = ct.Store.Delete(cloudlink.SecretName); err != nil {
			return fail(stderr, fmt.Errorf("removed %s, but not its Meko Cloud key: %w", l.Root, err)), false
		}
	}
	if keep {
		fmt.Fprintf(stdout, "Removed Meko's data in %s, and kept the extensions' data in it\n", l.Root)
		fmt.Fprintln(stdout, "The extensions' Meko key no longer works: after meko start, run meko "+
			"extension install agentk to point AgentK at the new Meko")
	} else {
		fmt.Fprintf(stdout, "Removed %s\n", l.Root)
	}
	if m != nil {
		fmt.Fprintf(stdout, "The %s service is still installed, and starts Meko on an empty data folder at the "+
			"next login: meko uninstall removes it\n", m.Kind())
	}
	if exe, err := os.Executable(); err == nil {
		fmt.Fprintf(stdout, "The meko binary is still at %s\n", exe)
	}
	fmt.Fprintln(stdout, "If you connected an agent to Meko, its token no longer works: after meko start, "+
		"run meko connect <agent> again (claude, codex, kiro or opencode). For Claude Code that also removes "+
		"the meko server of a folder's own (local scope) for this Meko, which Claude Code would use there instead")
	return exitOK, keep
}

// destroyOnlyExtensions handles a data folder that holds extension data but no meko.db (AgentK
// installed without Meko makes its home there): there's nothing of Meko's to remove, and with
// o.withExtensions it uninstalls the extensions and removes what's left of their data, and the
// folder only if that leaves it empty.
func destroyOnlyExtensions(l paths.Layout, exts []extensionData, o destroyOptions, in io.Reader, stdout,
	stderr io.Writer) int {
	if !o.withExtensions {
		fmt.Fprintf(stdout, "Nothing of Meko's to remove: %s holds only extension data (meko destroy "+
			"--with-extensions removes it too)\n", l.Root)
		return exitOK
	}
	listExtensionData(exts, o, stdout)
	if !o.force && !confirm(newAnswerReader(in), stdout, "Destroy it? [y/N] ") {
		fmt.Fprintln(stderr, "Nothing was removed")
		return exitError
	}
	if err := uninstallExtensions(l, exts, true, stdout, stderr); err != nil {
		return fail(stderr, err)
	}
	for _, p := range []string{l.Extensions, extensions.File(l.Root)} {
		if err := os.RemoveAll(p); err != nil {
			return fail(stderr, err)
		}
	}
	if os.Remove(l.Root) == nil {
		fmt.Fprintf(stdout, "Removed %s\n", l.Root)
	} else {
		fmt.Fprintf(stdout, "Removed the extensions' data in %s\n", l.Root)
	}
	return exitOK
}

// findExtensionData lists the extensions with data in the data folder: the registered ones, and the
// known ones whose folder is there.
func findExtensionData(l paths.Layout) ([]extensionData, error) {
	reg, err := extensions.Load(l.Root)
	if err != nil {
		return nil, err
	}
	byName := map[string]extensionData{}
	for _, n := range reg.Names() {
		byName[n] = extensionData{name: n, command: reg.Entries[n].Command, registered: true}
	}
	for n := range extensions.Known {
		if fi, serr := os.Stat(extensions.Dir(l.Root, n)); serr == nil && fi.IsDir() {
			e := byName[n]
			e.name = n
			byName[n] = e
		}
	}
	var out []extensionData
	for _, e := range byName {
		if fi, serr := os.Stat(extensions.Dir(l.Root, e.name)); serr == nil && fi.IsDir() {
			e.dir = extensions.Dir(l.Root, e.name)
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// listExtensionData says what extension data the folder holds, and what happens to it.
func listExtensionData(exts []extensionData, o destroyOptions, stdout io.Writer) {
	if len(exts) == 0 {
		return
	}
	fmt.Fprintln(stdout, "It also holds extension data:")
	for _, e := range exts {
		title := e.name
		if cat, ok := extensions.Known[e.name]; ok {
			title = cat.Title
		}
		if e.dir != "" {
			fmt.Fprintf(stdout, "  %s in %s (%s)\n", title, e.dir, humanSize(paths.DirSize(e.dir)))
		} else {
			fmt.Fprintf(stdout, "  %s (%s)\n", title, e.command)
		}
	}
	switch {
	case o.withExtensions:
		fmt.Fprintln(stdout, "--with-extensions uninstalls them and deletes their data too: AgentK's "+
			"sessions and worktrees, with any uncommitted work in them")
	case o.force:
		fmt.Fprintln(stdout, "--force keeps them, and their data")
	default:
		fmt.Fprintln(stdout, "Removing their data too uninstalls them and deletes AgentK's sessions and "+
			"worktrees, with any uncommitted work in them")
	}
}

// uninstallExtensions runs each extension's own uninstall, with its output shown: with purge,
// extensions.Catalog.UninstallArgs, which also deletes its data, else RemoveArgs, which keeps it. The
// first failure stops it, before anything of Meko's is touched.
func uninstallExtensions(l paths.Layout, exts []extensionData, purge bool, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	for _, e := range exts {
		cat, ok := extensions.Known[e.name]
		args := cat.RemoveArgs
		if purge {
			args = cat.UninstallArgs
		}
		if !ok || len(args) == 0 {
			fmt.Fprintf(stderr, "meko: Meko doesn't know how to uninstall extension %s\n", e.name)
			continue
		}
		cmd := e.command
		if _, serr := os.Stat(cmd); cmd == "" || serr != nil {
			cmd = cat.Find(e.name, home, l.Root)
		}
		if cmd == "" {
			fmt.Fprintf(stderr, "meko: %s's command isn't on PATH, in ~/.local/bin or in its folder, so its own "+
				"uninstall can't run; any service it installed stays\n", cat.Title)
			continue
		}
		fmt.Fprintf(stdout, "Uninstalling %s (%s %s)\n", cat.Title, cmd, strings.Join(args, " "))
		if err := (extensions.Entry{Command: cmd}).Uninstall(l.Root, args, stdout, stderr); err != nil {
			return fmt.Errorf("uninstalling %s failed: %w. Nothing of Meko's was removed", cat.Title, err)
		}
		if cat.Managed {
			// The link Meko made into its releases goes too: they're gone with a purge, and extension
			// delete removes them next.
			_ = cat.App(l.Root, e.name).UnlinkCommand(home)
		}
	}
	return nil
}

// removeDataDir removes a stopped server's data folder. It holds the lock while it renames the
// folder to a sibling, so no server can start in it, then removes the renamed folder. If the rename
// fails, nothing is removed: removing the folder in place would unlink the lock file partway, and
// flock locks the inode, so a serve starting then could lock a new file and write into the folder.
func removeDataDir(l paths.Layout) error {
	lock, err := lockForRemoval(l)
	if err != nil {
		return err
	}
	away := fmt.Sprintf("%s.destroying-%d", l.Root, os.Getpid())
	err = rename(l.Root, away)
	releaseLock(lock)
	if err != nil {
		return fmt.Errorf("can't move %s aside to remove it (the folder it's in must be writable): %w. "+
			"Nothing was removed", l.Root, err)
	}
	if err = os.RemoveAll(away); err != nil {
		return fmt.Errorf("the data folder was moved to %s, but removing it failed partway: %w", away, err)
	}
	return nil
}

// keptEntries are the data folder's entries destroy leaves when it keeps the extensions' data.
var keptEntries = map[string]bool{"extensions": true, "extensions.json": true}

// removeKeepingExtensions removes a stopped server's data but the extensions' (keptEntries). The
// folder itself stays, since an extension may be running from its folder in it (AgentK's server and
// sessions). Holding the lock, it moves every other entry into a sibling folder, run/ (which holds
// the lock file) last, so no server can start until Meko's data is all gone; a server that starts
// after that makes a new folder's worth. Then it removes the sibling. If a move fails, it moves back
// what it moved, and nothing is removed.
func removeKeepingExtensions(l paths.Layout) error {
	lock, err := lockForRemoval(l)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(l.Root)
	if err != nil {
		releaseLock(lock)
		return err
	}
	var names []string
	for _, e := range entries {
		if !keptEntries[e.Name()] && e.Name() != "run" {
			names = append(names, e.Name())
		}
	}
	names = append(names, "run")
	away := fmt.Sprintf("%s.destroying-%d", l.Root, os.Getpid())
	if err = os.Mkdir(away, 0o700); err != nil {
		releaseLock(lock)
		return fmt.Errorf("can't make %s to move Meko's data aside (the folder it's in must be writable): %w. "+
			"Nothing was removed", away, err)
	}
	var moved []string
	for _, n := range names {
		if err = rename(filepath.Join(l.Root, n), filepath.Join(away, n)); err != nil {
			for i := len(moved) - 1; i >= 0; i-- {
				_ = rename(filepath.Join(away, moved[i]), filepath.Join(l.Root, moved[i]))
			}
			_ = os.Remove(away)
			releaseLock(lock)
			return fmt.Errorf("can't move %s aside to remove it: %w. Nothing was removed", filepath.Join(l.Root, n), err)
		}
		moved = append(moved, n)
	}
	releaseLock(lock)
	if err = os.RemoveAll(away); err != nil {
		return fmt.Errorf("the data was moved to %s, but removing it failed partway: %w", away, err)
	}
	return nil
}

// rename is os.Rename; tests replace it.
var rename = os.Rename

// lockForRemoval takes the lock of a stopped server's data folder, so no server starts in it.
func lockForRemoval(l paths.Layout) (*os.File, error) {
	if err := os.MkdirAll(l.RunDir, 0o700); err != nil { // a folder with only meko.db has no lock file
		return nil, err
	}
	lock, err := acquireServeLock(l.Lock)
	if errors.Is(err, errLocked) {
		return nil, errors.New("a server started in the data folder while it was being destroyed. Nothing was " +
			"removed; run meko destroy again")
	}
	return lock, err
}

// answerReader reads a person's answers, one line each, from one buffer, so a second question
// gets the second line; term says whether a terminal echoes them.
type answerReader struct {
	*bufio.Reader
	term bool
}

func newAnswerReader(in io.Reader) *answerReader {
	if a, ok := in.(*answerReader); ok {
		return a
	}
	return &answerReader{bufio.NewReader(in), isTerminal(in)}
}

// confirm asks question and reports whether the answer is y or yes. End of input is no.
func confirm(in io.Reader, stdout io.Writer, question string) bool {
	a := newAnswerReader(in)
	fmt.Fprint(stdout, question)
	line, err := a.ReadString('\n')
	if err != nil || !a.term {
		fmt.Fprintln(stdout) // only a terminal echoes the answer's newline, and only if it came
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes"
}

// isTerminal reports whether v (a reader or writer) is a terminal (spin.IsTerminal).
func isTerminal(v any) bool { return spin.IsTerminal(v) }

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}
