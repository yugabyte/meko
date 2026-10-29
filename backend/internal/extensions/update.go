package extensions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/yugabyte/meko/backend/internal/spin"
	"github.com/yugabyte/meko/backend/internal/update"
)

// UpdateOptions say where an update runs: the data folder at Root, the user's Home, the releases
// Client, and where its output goes. Releases, when set, are the releases a check just listed, so an
// update after it doesn't list them again.
type UpdateOptions struct {
	Root     string
	Home     string
	Client   *update.Client
	Releases []update.Release
	Check    bool // only say whether there's an update
	Out      io.Writer
}

// ErrNotUpdatable is why Update refuses an extension it can't update.
var ErrNotUpdatable = errors.New("not a release Meko installed")

// Update updates a Managed extension's release install to its newest release (its bad versions
// skipped), or with o.Check only says whether there is one. The caller holds the update lock
// (update.Lock). It deletes the versions but the current one, the one to roll back to and the ones
// in use, stages the new one beside them (checked with version --json and the catalog's CheckArgs),
// and switches app/current, recording the update (update-pending.json) for meko _launch, which
// counts its starts. A running extension with RestartArgs is switched while it runs and then
// restarted by them; one without is stopped, switched, and started up to MaxFailedStarts+1 times.
// Either way, app/current naming the old version afterwards means meko _launch rolled it back, which
// is an error here. It reports whether the extension runs afterwards.
func Update(ctx context.Context, o UpdateOptions, name string) (bool, error) {
	out := o.Out
	cat, ok := Known[name]
	if !ok {
		return false, fmt.Errorf("no extension called %q: meko knows %s", name, strings.Join(KnownNames(), ", "))
	}
	if !cat.Managed {
		return false, fmt.Errorf("%s isn't installed from releases, so Meko can't update it", cat.Title)
	}
	reg, err := Load(o.Root)
	if err != nil {
		return false, err
	}
	e, ok := reg.Entries[name]
	if !ok {
		return false, fmt.Errorf("%s isn't installed: meko extension install %s installs it", cat.Title, name)
	}
	if _, serr := os.Stat(e.Command); errors.Is(serr, os.ErrNotExist) {
		if reg.FindAgain(name, o.Home, o.Root) == "" {
			return false, fmt.Errorf("%s %s", cat.Title, Damaged(name, e.Command))
		}
		e = reg.Entries[name]
	}
	cur := cat.Release(o.Root, name, e.Command)
	if cur == "" {
		return false, fmt.Errorf("%w: %s", ErrNotUpdatable, cat.WhyNotUpdatable(o.Root, o.Home, name, e.Command))
	}
	rels := o.Releases
	if rels == nil {
		looking := spin.Wait(out, "Looking for "+cat.Title+"'s releases")
		rels, err = o.Client.Releases(ctx)
		looking.Clear()
		if err != nil {
			return false, err
		}
	}
	app := cat.App(o.Root, name)
	goos, arch := update.Platform()
	rel, next, found := update.PickPackage(rels, cat.TagPrefixes, name, goos, arch, app.BadVersions())
	if !found {
		fmt.Fprintf(out, "%s %s is installed; no release of it for %s-%s was found in %s, leaving out versions "+
			"that were rolled back\n", cat.Title, cur, goos, arch, o.Client.Repo)
		return false, nil
	}
	cv, _ := update.ParseVersion(cur)
	nv, _ := update.ParseVersion(next)
	if !cv.Less(nv) {
		fmt.Fprintf(out, "%s %s is the newest release\n", cat.Title, cur)
		return false, nil
	}
	if o.Check {
		fmt.Fprintf(out, "%s %s is available (this is %s): meko extension update %s installs it\n", cat.Title, next,
			cur, name)
		return false, nil
	}
	// The version to roll back to is the current one, unless that's still on probation (its update
	// hasn't served 5 minutes yet): then it's the one that update replaced, the last known to work.
	from := cur
	if p, _ := app.ReadPending(); p != nil && p.To == cur {
		_, isVersion := update.ParseVersion(p.From)
		//nolint:gosec // G703: p.From is checked to be a bare version, a folder name in the app folder
		if _, serr := os.Stat(app.Exe(p.From)); isVersion && serr == nil {
			from = p.From
			fmt.Fprintf(out, "%s %s hasn't served 5 minutes yet, so %s stays the version to roll back to\n",
				cat.Title, cur, from)
		}
	}
	// The other installed versions go, but for one a process still runs from. Prune deletes only
	// version folders: a .staging- or .download- folder that a killed update left behind stays.
	app.Prune(cur, from)
	installing := spin.Start(out, "Release", fmt.Sprintf("installing %s %s (%s)", cat.Title, next, rel.Tag))
	if err = app.Stage(ctx, o.Client, rel, next); err != nil {
		installing.Fail()
		return false, err
	}
	installing.Done(fmt.Sprintf("%s %s (%s), downloaded and checked", cat.Title, next, rel.Tag))
	up, _ := e.Status(o.Root)
	if len(cat.RestartArgs) > 0 {
		return restartInto(o.Root, cat, e, app, from, next, up, out)
	}
	if up {
		if err = e.Run(o.Root, "stop", out, out); err != nil {
			return false, err
		}
	}
	if err = app.Switch(from, next, 0); err != nil {
		// app/current still names the old version: start it again rather than leave it stopped.
		restarted := up && e.Run(o.Root, "start", out, out) == nil
		return restarted, fmt.Errorf("can't switch to %s: %w", next, err)
	}
	if !up {
		fmt.Fprintf(out, "Installed %s %s; it runs at its next start\n", cat.Title, next)
		return false, nil
	}
	for range update.MaxFailedStarts + 1 {
		if err = e.Run(o.Root, "start", out, out); err == nil {
			break
		}
	}
	return started(cat, app, next, err, out)
}

// restartInto switches a running extension to next and restarts it with its RestartArgs, which
// return once it answers: the new version, or the old one meko _launch rolled back to.
func restartInto(root string, cat Catalog, e Entry, app update.App, from, next string, up bool,
	out io.Writer) (bool, error) {
	if err := app.Switch(from, next, 0); err != nil {
		return up, fmt.Errorf("can't switch to %s: %w", next, err)
	}
	if !up {
		fmt.Fprintf(out, "Installed %s %s; it runs at its next start\n", cat.Title, next)
		return false, nil
	}
	err := e.RunArgs(root, cat.RestartArgs, RestartTimeout, out, out)
	return started(cat, app, next, err, out)
}

// RestartTimeout bounds an extension's RestartArgs: long enough for meko _launch to roll back a
// version that fails to start.
var RestartTimeout = 3 * StartTimeout / 2

// started reports how a start of next went: running, rolled back (app/current names another
// version), or failed with err.
func started(cat Catalog, app update.App, next string, err error, out io.Writer) (bool, error) {
	if now := app.Current(); now != next {
		if err != nil {
			return false, fmt.Errorf("%s %s failed to start %d times, so it was rolled back to %s, which didn't "+
				"start either: %w. See %s status", cat.Title, next, update.MaxFailedStarts, now, err, cat.Command)
		}
		return true, fmt.Errorf("%s %s failed to start %d times, so it was rolled back to %s and won't be "+
			"offered again", cat.Title, next, update.MaxFailedStarts, now)
	}
	if err != nil {
		return false, fmt.Errorf("%s %s didn't start: %w. See %s status", cat.Title, next, err, cat.Command)
	}
	fmt.Fprintf(out, "%s is running %s\n", cat.Title, next)
	return true, nil
}

// WhyNotUpdatable says why Meko can't update an installed extension whose command isn't a release
// it installed (Release is ""): one still in an old home, or a build from source.
func (c Catalog) WhyNotUpdatable(root, home, name, command string) string {
	if h := c.oldHomeOf(home, command); h != "" {
		return fmt.Sprintf("%s still runs from %s, and Meko updates it once it has moved into Meko's data folder "+
			"(%s), which it does by itself at its next start", c.Title, h, Dir(root, name))
	}
	return fmt.Sprintf("%s here (%s) isn't a release Meko installed, so Meko can't update it: update the build "+
		"yourself, or remove it from PATH and run meko extension install %s", c.Title, command, name)
}

// oldHomeOf is the old home (OldHomes, under home) command runs a release from, or "".
func (c Catalog) oldHomeOf(home, command string) string {
	real, err := filepath.EvalSymlinks(command)
	if err != nil {
		return ""
	}
	for _, h := range c.OldHomes {
		dir := filepath.Join(home, h)
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			dir = r
		}
		if strings.HasPrefix(real, dir+string(filepath.Separator)) {
			return dir
		}
	}
	return ""
}

// Check reads, from the releases a check listed, the newest release of each registered extension
// Meko updates (its bad versions left out), for update.Updater.Extensions.
func Check(root string, releases []update.Release) map[string]update.ExtensionState {
	reg, err := Load(root)
	if err != nil {
		return nil
	}
	out := map[string]update.ExtensionState{}
	goos, arch := update.Platform()
	for _, name := range reg.Names() {
		cat, ok := Known[name]
		if !ok || !cat.Managed {
			continue
		}
		app := cat.App(root, name)
		rel, ver, found := update.PickPackage(releases, cat.TagPrefixes, name, goos, arch, app.BadVersions())
		if !found {
			out[name] = update.ExtensionState{}
			continue
		}
		out[name] = update.ExtensionState{Latest: ver, LatestURL: rel.URL}
	}
	return out
}

// Newer reports whether latest is a newer version than current.
func Newer(current, latest string) bool {
	cv, ok1 := update.ParseVersion(current)
	lv, ok2 := update.ParseVersion(latest)
	return ok1 && ok2 && cv.Less(lv)
}
