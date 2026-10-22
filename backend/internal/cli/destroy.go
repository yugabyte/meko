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
// symlinked data folder loses the folder it points to and then the link.
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
		return fail(stderr, fmt.Errorf("%s doesn't look like Meko's data folder (it has no meko.db or "+
			"run/meko.lock), so it wasn't removed", l.Root))
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

	msg, err := stopServer(l)
	if err != nil {
		return fail(stderr, fmt.Errorf("%w. Nothing was removed", err))
	}
	if up {
		fmt.Fprintln(stdout, msg)
	}
	if err = removeDataDir(l); err != nil {
		return fail(stderr, err)
	}
	if link != "" {
		if err = paths.RemoveIfExists(link); err != nil {
			return fail(stderr, fmt.Errorf("removed %s, but not the symlink %s to it: %w", l.Root, link, err))
		}
	}
	fmt.Fprintf(stdout, "Removed %s.\n", l.Root)
	if exe, err := os.Executable(); err == nil {
		fmt.Fprintf(stdout, "The meko binary is still at %s. ", exe)
	}
	fmt.Fprintln(stdout, "If you registered Meko with Claude Code, remove it there too: claude mcp remove meko")
	return exitOK
}

// removeDataDir removes a stopped server's data folder. It holds the lock while it renames the
// folder to a sibling, so no server can start in it, then removes the renamed folder. If the rename
// fails, it removes the folder in place, still holding the lock.
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
	if err = os.Rename(l.Root, away); err != nil {
		err = os.RemoveAll(l.Root)
		releaseLock(lock)
		if err != nil {
			return fmt.Errorf("removing %s failed partway, so some of it is left: %w", l.Root, err)
		}
		return nil
	}
	releaseLock(lock)
	if err = os.RemoveAll(away); err != nil {
		return fmt.Errorf("the data folder was moved to %s, but removing it failed partway: %w", away, err)
	}
	return nil
}

// confirm asks question and reports whether the answer is y or yes. End of input is no.
func confirm(in io.Reader, stdout io.Writer, question string) bool {
	fmt.Fprint(stdout, question)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		fmt.Fprintln(stdout) // the answer's own newline never came
	}
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
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
