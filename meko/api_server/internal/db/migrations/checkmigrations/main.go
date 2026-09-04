// Command checkmigrations checks every migration file this git branch adds
// or renumbers against another ref's current tip (main, by default):
//
//   - collision: the new file's (version, direction) matches an existing
//     file's under a different name. golang-migrate refuses to load a
//     directory with two such files at all, aborting startup on every
//     environment before RunMigrations ever compares against the DB's
//     applied version.
//   - gap: the new versions in a directory don't extend that ref's current
//     highest version by exactly 1, 2, 3, ... in order. A gap is usually
//     the same mistake in the other direction -- whoever picked the number
//     didn't check what the ref already had, and guessed too high.
//   - unpaired: a version in this branch's own tree has an .up.sql with no
//     matching .down.sql, or vice versa. An up with no down can never be
//     rolled back; a down with no up can never be reached in the first
//     place -- either way golang-migrate's directory listing is missing a
//     file it expects to find once that version is current.
//
// A collision is checked against two sources per directory: the files
// already on baseRef (catches a same-numbered file some other,
// already-merged PR added, which this branch's own history never contains
// until it merges -- needs diffing against main's tip fetched fresh in CI,
// via `git fetch origin main` done by the caller before this runs) and this
// same diff's own other added files (catches two migrations added together
// in one PR that duplicate each other -- the exact shape of the bug that
// motivated this tool).
//
// checkAgainstRef's checks above are all git-diff-based, so they only see
// what this branch changed -- they're a no-op on the `push: [main]` trigger,
// where HEAD already equals origin/main right after a merge. This package's
// own TestNoDuplicateVersionsInTree (main_test.go) complements that: it
// loads each migration directory through golang-migrate's real loader
// pointed at the files on disk, so it still catches a collision already
// sitting in one tree regardless of git history.
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// migrationName matches golang-migrate's own filename convention
// (source.Regex in github.com/golang-migrate/migrate/v4/source): a leading
// version number, then "_", a title, then ".up." or ".down.". Direction is
// what makes an .up.sql/.down.sql pair for the SAME version not a collision
// -- only two files sharing both fields are.
var migrationName = regexp.MustCompile(`^([0-9]+)_.*\.(up|down)\.`)

// migrationKey identifies one migration file's (version, direction) pair --
// what golang-migrate actually requires to be unique, not just the version
// number alone.
type migrationKey struct {
	version    int
	versionStr string // preserves zero-padding ("000014") for messages
	direction  string
}

func parseMigrationKey(filename string) (migrationKey, bool) {
	m := migrationName.FindStringSubmatch(filename)
	if m == nil {
		return migrationKey{}, false
	}
	v, err := strconv.Atoi(m[1])
	if err != nil {
		return migrationKey{}, false
	}
	return migrationKey{version: v, versionStr: m[1], direction: m[2]}, true
}

func main() {
	baseRef := "origin/main"
	if len(os.Args) > 1 {
		baseRef = os.Args[1]
	}
	root := "meko/api_server/internal/db/migrations"
	if len(os.Args) > 2 {
		root = os.Args[2]
	}
	fmt.Printf("checkmigrations: comparing %s against %s\n", root, baseRef)

	problems, err := checkAgainstRef(baseRef, root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(problems) == 0 {
		fmt.Printf("checkmigrations: No migration version problems against %s.\n", baseRef)
		return
	}
	fmt.Printf("checkmigrations: %d problem(s) found against %s:\n", len(problems), baseRef)
	for _, p := range problems {
		fmt.Printf("::error::%s\n", p)
	}
	fmt.Fprintf(os.Stderr,
		"\nOne or more new migrations conflict with %s's current versions -- "+
			"rebase onto %s and renumber to the next free version in the affected directory.\n",
		baseRef, baseRef)
	os.Exit(1)
}

// checkAgainstRef is the only piece of this file that touches git; the
// comparisons themselves (findCollisions, findGaps) are pure functions of
// version lists, so they're unit-testable without a real repository.
func checkAgainstRef(baseRef, root string) ([]string, error) {
	// "rev-parse --show-toplevel" prints the absolute path of the repo root,
	// e.g. "/home/runner/work/meko/meko". Every other git call below runs
	// with "-C" pointed at this, so it works the same whether the caller's
	// cwd is the repo root or, as in CI, meko/api_server.
	repoRoot, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("find repo root: %w", err)
	}
	repoRoot = strings.TrimSpace(repoRoot)

	// "merge-base HEAD <ref>" prints the SHA where this branch actually
	// diverged from baseRef, e.g. "c43d39d65e0a1d08f097f7b23c6138eb65f858e8".
	// Diffing from there (not from baseRef's tip directly) means a migration
	// this branch renamed on its own history doesn't show up as "added" a
	// second time just because baseRef has since moved past that rename.
	mergeBase, err := git("-C", repoRoot, "merge-base", "HEAD", baseRef)
	if err != nil {
		return nil, fmt.Errorf("merge-base HEAD %s: %w", baseRef, err)
	}
	mergeBase = strings.TrimSpace(mergeBase)
	fmt.Printf("checkmigrations: repo root %s, merge-base(HEAD, %s) = %s\n", repoRoot, baseRef, mergeBase)

	// "diff --name-status --diff-filter=AR <mergeBase> HEAD -- <root>" lists
	// only files Added or Renamed between the merge-base and this branch's
	// tip, one per line as "A\t<path>" or "R100\t<old path>\t<new path>" --
	// e.g. "A\tmeko/api_server/internal/db/migrations/system/000014_x.up.sql".
	// sqlPathsFrom takes the last tab-separated field of each line, which is
	// the new path for a rename and the whole line for a plain add.
	added, err := sqlPathsFrom(repoRoot, "diff", "--name-status", "--diff-filter=AR", mergeBase, "HEAD", "--", root)
	if err != nil {
		return nil, fmt.Errorf("diff added/renamed migrations: %w", err)
	}
	if len(added) == 0 {
		fmt.Printf("checkmigrations: no added/renamed migration files under %s since merge-base -- nothing to check\n", root)
		return nil, nil
	}

	addedByDir := map[string][]string{}
	for _, f := range added {
		dir := path.Dir(f)
		addedByDir[dir] = append(addedByDir[dir], f)
	}

	var problems []string
	for dir, addedFiles := range addedByDir {
		// "ls-tree -r --name-only <ref> -- <dir>" lists every file <ref> has
		// under dir, one path per line, e.g.
		// "meko/api_server/internal/db/migrations/system/000013_x.up.sql" --
		// baseRef's whole tree there, not just what this branch touched, so
		// a collision with a file this branch never opened is still caught.
		existing, err := sqlPathsFrom(repoRoot, "ls-tree", "-r", "--name-only", baseRef, "--", dir)
		if err != nil {
			return nil, fmt.Errorf("ls-tree %s at %s: %w", dir, baseRef, err)
		}

		// Same as above but for "HEAD" instead of baseRef: this branch's own
		// current, final list of files in dir. Used only for the up/down
		// pairing check, which cares about the tree this branch is actually
		// proposing to ship, not what's changing relative to anything else.
		headTree, err := sqlPathsFrom(repoRoot, "ls-tree", "-r", "--name-only", "HEAD", "--", dir)
		if err != nil {
			return nil, fmt.Errorf("ls-tree %s at HEAD: %w", dir, err)
		}

		fmt.Printf("checkmigrations: [%s] %d added/renamed: %s\n", dir, len(addedFiles), strings.Join(addedFiles, ", "))
		fmt.Printf("checkmigrations: [%s] %d existing on %s: %s\n", dir, len(existing), baseRef, strings.Join(existing, ", "))
		dirProblems := checkDir(dir, addedFiles, existing, headTree, baseRef)
		if len(dirProblems) == 0 {
			fmt.Printf("checkmigrations: [%s] no problems\n", dir)
		}
		problems = append(problems, dirProblems...)
	}
	return problems, nil
}

// checkDir runs the collision, gap, and unpaired checks for one migrations
// directory -- pulled out of checkAgainstRef so it's testable without a real
// git repo. Each added file is checked against two sources: existing
// (baseRef's tree, for a collision with something this branch never
// touched) and addedFiles itself (for two migrations this same diff adds
// that duplicate each other -- findGaps alone can't catch that, since it
// dedups by version and so sees one legitimate-looking step where there are
// really two colliding files). headTree is this branch's own final state of
// dir, used only to check every version has both an up.sql and a down.sql.
func checkDir(dir string, addedFiles, existing, headTree []string, baseRef string) []string {
	var problems []string
	for _, file := range addedFiles {
		k, ok := parseMigrationKey(path.Base(file))
		if !ok {
			fmt.Printf("checkmigrations: [%s] %s doesn't match golang-migrate's filename convention, skipping\n", dir, file)
			continue
		}
		fmt.Printf("checkmigrations: [%s] checking %s (version=%s direction=%s)\n", dir, file, k.versionStr, k.direction)

		against := findCollisions(file, existing, baseRef)
		fmt.Printf("checkmigrations: [%s] %s vs %d existing on %s: %d collision(s)\n",
			dir, file, len(existing), baseRef, len(against))
		problems = append(problems, against...)

		withinPR := findCollisions(file, addedFiles, "this PR")
		fmt.Printf("checkmigrations: [%s] %s vs %d other added file(s) in this PR: %d collision(s)\n",
			dir, file, len(addedFiles)-1, len(withinPR))
		problems = append(problems, withinPR...)
	}

	gaps := findGaps(dir, addedFiles, existing, baseRef)
	fmt.Printf("checkmigrations: [%s] gap check against %s: %d gap(s)\n", dir, baseRef, len(gaps))
	problems = append(problems, gaps...)

	unpaired := findUnpaired(dir, headTree)
	fmt.Printf("checkmigrations: [%s] up/down pairing check over %d file(s) at HEAD: %d unpaired\n",
		dir, len(headTree), len(unpaired))
	problems = append(problems, unpaired...)

	return problems
}

// findCollisions reports every entry in existing that shares file's
// (version, direction) but not its exact name.
func findCollisions(file string, existing []string, baseRef string) []string {
	k, ok := parseMigrationKey(path.Base(file))
	if !ok {
		return nil
	}
	var problems []string
	for _, other := range existing {
		otherBase := path.Base(other)
		if otherBase == path.Base(file) {
			continue // same file, not a collision
		}
		otherKey, ok := parseMigrationKey(otherBase)
		if ok && otherKey == k {
			problems = append(problems, fmt.Sprintf(
				"%s: version %s (%s) collides with %s, already on %s -- "+
					"renumber this file to the next free version in %s",
				file, k.versionStr, k.direction, other, baseRef, path.Dir(file)))
		}
	}
	return problems
}

// findGaps reports when addedFiles' versions don't extend existing's highest
// version by exactly 1, 2, 3, ... in order. up/down siblings share a
// version, so they collapse to one step each, not two.
func findGaps(dir string, addedFiles, existing []string, baseRef string) []string {
	maxExisting := 0
	for _, f := range existing {
		if k, ok := parseMigrationKey(path.Base(f)); ok && k.version > maxExisting {
			maxExisting = k.version
		}
	}

	seen := map[int]bool{}
	var addedVersions []int
	for _, f := range addedFiles {
		if k, ok := parseMigrationKey(path.Base(f)); ok && !seen[k.version] {
			seen[k.version] = true
			addedVersions = append(addedVersions, k.version)
		}
	}
	sort.Ints(addedVersions)

	var problems []string
	want := maxExisting
	for _, got := range addedVersions {
		want++
		if got != want {
			problems = append(problems, fmt.Sprintf(
				"%s: new version %d skips ahead of %s's highest version %d -- "+
					"expected the next migration to be %d, not %d",
				dir, got, baseRef, maxExisting, want, got))
			want = got // one gap reported per jump, not one per file after it
		}
	}
	return problems
}

// findUnpaired reports every version in files whose up.sql and down.sql
// don't both exist -- golang-migrate can apply an up migration with no
// down.sql (it just can't ever be rolled back), and a down.sql with no
// up.sql is dead code no version ever reaches, so neither is a load error
// like a collision, but both leave a version this repo can never safely
// undo or never runs at all.
func findUnpaired(dir string, files []string) []string {
	haveDirection := map[string]map[string]string{} // versionStr -> direction -> filename
	for _, f := range files {
		k, ok := parseMigrationKey(path.Base(f))
		if !ok {
			continue
		}
		if haveDirection[k.versionStr] == nil {
			haveDirection[k.versionStr] = map[string]string{}
		}
		haveDirection[k.versionStr][k.direction] = f
	}

	var versions []string
	for v := range haveDirection {
		versions = append(versions, v)
	}
	sort.Strings(versions)

	var problems []string
	for _, v := range versions {
		byDirection := haveDirection[v]
		up, hasUp := byDirection["up"]
		down, hasDown := byDirection["down"]
		switch {
		case hasUp && !hasDown:
			problems = append(problems, fmt.Sprintf(
				"%s: version %s has %s but no matching down.sql -- it can never be rolled back",
				dir, v, up))
		case hasDown && !hasUp:
			problems = append(problems, fmt.Sprintf(
				"%s: version %s has %s but no matching up.sql -- it will never run",
				dir, v, down))
		}
	}
	return problems
}

// sqlPathsFrom runs a git subcommand that emits one path (or
// "status\told\tnew") per line and returns the .sql-suffixed destination
// path of each line -- the last tab-separated field, which for a plain path
// is the whole line and for `diff --name-status`'s rename rows is the new
// path.
func sqlPathsFrom(repoRoot string, gitArgs ...string) ([]string, error) {
	out, err := git(append([]string{"-C", repoRoot}, gitArgs...)...)
	if err != nil {
		return nil, err
	}
	var files []string
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		dest := fields[len(fields)-1]
		if strings.HasSuffix(dest, ".sql") {
			files = append(files, dest)
		}
	}
	return files, scanner.Err()
}

func git(args ...string) (string, error) {
	fmt.Printf("checkmigrations: $ git %s\n", strings.Join(args, " "))
	cmd := exec.Command("git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}
