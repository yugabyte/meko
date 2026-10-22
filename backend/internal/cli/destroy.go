package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/yugabyte/meko/backend/internal/paths"
)

// runDestroy stops the server and removes the whole data folder, after asking (unless force).
// Only a folder that looks like Meko's is removed. The folder is renamed away while the lock is
// held, so a start at the same moment finds either all of it or none of it, and then removed. A
// symlinked data folder loses the folder it points to and then the link. A missing or empty folder
// is nothing to remove, and is left as it is (a link to it too).
func runDestroy(l paths.Layout, force bool, in io.Reader, stdout, stderr io.Writer) int {
	link := ""
	fi, err := os.Lstat(l.Root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		fmt.Fprintf(stdout, "Nothing to remove: there's no data folder at %s.\n", l.Root)
		return exitOK
	case err != nil:
		return fail(stderr, err)
	case fi.Mode()&fs.ModeSymlink != 0:
		target, lerr := filepath.EvalSymlinks(l.Root)
		if lerr != nil {
			return fail(stderr, fmt.Errorf("the data folder %s is a symlink that can't be followed: %w", l.Root, lerr))
		}
		link, l = l.Root, paths.New(target)
	}
	// Resolving the default folder on an NFS home makes /var/tmp/<user>/meko, so an empty one is
	// nothing to remove rather than a folder that isn't Meko's.
	if entries, rerr := os.ReadDir(l.Root); rerr == nil && len(entries) == 0 {
		fmt.Fprintf(stdout, "Nothing to remove: the data folder %s is empty.\n", l.Root)
		return exitOK
	}
	if !paths.LooksLikeDataDir(l.Root) {
		return fail(stderr, fmt.Errorf("%s doesn't look like Meko's data folder (it has no meko.db, which "+
			"only a server makes), so it wasn't removed", l.Root))
	}
	up, err := running(l.Lock)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "This removes Meko's data in %s (%s) for good: every datapack, memory and "+
		"conversation, the config, the client token and the logs.\n", l.Root, humanSize(paths.DirSize(l.Root)))
	if up {
		fmt.Fprintln(stdout, "Meko is running, so it's stopped first.")
	}
	if !force && !confirm(in, stdout, "Destroy it? [y/N] ") {
		fmt.Fprintln(stderr, "Nothing was removed.")
		return exitError
	}

	// Under its service, a server that only gets a signal is started again: stop the service first.
	m := serviceOf(l)
	if m != nil {
		if err = m.Stop(); err != nil {
			return fail(stderr, fmt.Errorf("%w. Nothing was removed", err))
		}
	}
	msg, err := stopServer(l)
	if err != nil {
		return fail(stderr, fmt.Errorf("%w. Nothing was removed", err))
	}
	if up {
		fmt.Fprintln(stdout, msg)
	}
	err = removeDataDir(l)
	// Once the folder has been moved away, the link points at nothing, even if removing it failed.
	if _, serr := os.Lstat(l.Root); link != "" && errors.Is(serr, fs.ErrNotExist) {
		if lerr := paths.RemoveIfExists(link); lerr != nil {
			err = errors.Join(err, fmt.Errorf("the symlink %s to it is left: %w", link, lerr))
		}
	}
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "Removed %s.\n", l.Root)
	if m != nil {
		fmt.Fprintf(stdout, "The %s service is still installed, and starts Meko on an empty data folder at the "+
			"next login: meko uninstall removes it.\n", m.Kind())
	}
	if exe, err := os.Executable(); err == nil {
		fmt.Fprintf(stdout, "The meko binary is still at %s.\n", exe)
	}
	fmt.Fprintln(stdout, "If you registered Meko with Claude Code, its token no longer works: remove it "+
		"(claude mcp remove meko), and after meko start, register again with the line meko token --mcp prints.")
	return exitOK
}

// removeDataDir removes a stopped server's data folder. It holds the lock while it renames the
// folder to a sibling, so no server can start in it, then removes the renamed folder. If the rename
// fails, nothing is removed: removing the folder in place would unlink the lock file partway, and
// flock locks the inode, so a serve starting then could lock a new file and write into the folder.
func removeDataDir(l paths.Layout) error {
	if err := os.MkdirAll(l.RunDir, 0o700); err != nil { // a folder with only meko.db has no lock file
		return err
	}
	lock, err := acquireServeLock(l.Lock)
	if errors.Is(err, errLocked) {
		return errors.New("a server started in the data folder while it was being destroyed. Nothing was " +
			"removed; run meko destroy again")
	}
	if err != nil {
		return err
	}
	away := fmt.Sprintf("%s.destroying-%d", l.Root, os.Getpid())
	err = os.Rename(l.Root, away)
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

// confirm asks question and reports whether the answer is y or yes. End of input is no.
func confirm(in io.Reader, stdout io.Writer, question string) bool {
	fmt.Fprint(stdout, question)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil || !isTerminal(in) {
		fmt.Fprintln(stdout) // only a terminal echoes the answer's newline, and only if it came
	}
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

// isTerminal reports whether r is a terminal (a character device), which echoes what's typed.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&fs.ModeCharDevice != 0
}

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
