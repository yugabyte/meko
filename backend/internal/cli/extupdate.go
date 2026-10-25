package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/extensions"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/update"
)

// runUpdateExtension is meko update_extension <name> [--check]. It needs no running server: an
// extension starts and stops by its own commands.
func runUpdateExtension(l paths.Layout, name string, check bool, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), extensions.InstallTimeout)
	defer cancel()
	if _, err := updateExtension(ctx, l, name, check, stdout); err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// updateExtension updates a Managed extension's release install to its newest release (its bad
// versions skipped), or with check only says whether there is one. It deletes the versions but the
// current one, stages the new one beside it, stops the extension if it runs, and switches
// app/current, recording the update (update-pending.json) for the extension's launcher. A running
// extension is started again, up to MaxFailedStarts+1 times: the launcher rolls back to the old
// version at the start after MaxFailedStarts failed ones, which is an error here. It reports whether
// the extension runs afterwards.
func updateExtension(ctx context.Context, l paths.Layout, name string, check bool, out io.Writer) (bool, error) {
	cat, ok := extensions.Known[name]
	if !ok {
		return false, fmt.Errorf("no extension called %q: meko knows %s", name, strings.Join(extensions.KnownNames(), ", "))
	}
	if !cat.Managed {
		return false, fmt.Errorf("%s updates itself: use the Update button in %s", cat.Title, cat.Title)
	}
	reg, err := extensions.Load(l.Root)
	if err != nil {
		return false, err
	}
	e, ok := reg.Entries[name]
	if !ok {
		return false, fmt.Errorf("%s isn't installed: meko install_extension %s installs it", cat.Title, name)
	}
	if _, serr := os.Stat(e.Command); errors.Is(serr, os.ErrNotExist) {
		if findAgain(l, reg, name) == "" {
			return false, fmt.Errorf("%s is gone (%s); meko uninstall_extension %s forgets it", name, e.Command, name)
		}
		e = reg.Entries[name]
	}
	cur := cat.Release(l.Root, name, e.Command)
	if cur == "" {
		return false, fmt.Errorf("%s here (%s) isn't a release Meko installed, so Meko can't update it: "+
			"update the build yourself, or remove it from PATH and run meko install_extension %s", cat.Title,
			e.Command, name)
	}
	cfg, err := config.Load(l.Config, os.Getenv, config.Overrides{})
	if err != nil {
		return false, err
	}
	c := newReleaseClient(cfg.Update.Repo)
	rels, err := c.Releases(ctx)
	if err != nil {
		return false, err
	}
	app := cat.App(l.Root, name)
	goos, arch := update.Platform()
	rel, next, found := update.PickPackage(rels, cat.TagPrefixes, cat.Command, goos, arch, app.BadVersions())
	cv, _ := update.ParseVersion(cur)
	nv, _ := update.ParseVersion(next)
	if !found || !cv.Less(nv) {
		fmt.Fprintf(out, "%s %s is the newest release\n", cat.Title, cur)
		return false, nil
	}
	if check {
		fmt.Fprintf(out, "%s %s is available (this is %s): meko update_extension %s installs it\n", cat.Title, next,
			cur, name)
		return false, nil
	}
	fmt.Fprintf(out, "Installing %s %s (%s)\n", cat.Title, next, rel.Tag)
	// Only the current version is kept to roll back to; older ones and a staging left behind go.
	app.Prune(cur)
	if err = app.Stage(ctx, c, rel, next); err != nil {
		return false, err
	}
	up, _ := e.Status(l.Root)
	if up {
		if err = e.Run(l.Root, "stop", out, out); err != nil {
			return false, err
		}
	}
	if err = app.Switch(cur, next, 0); err != nil {
		return false, fmt.Errorf("can't switch to %s: %w", next, err)
	}
	if !up {
		fmt.Fprintf(out, "Installed %s %s; it runs at its next start\n", cat.Title, next)
		return false, nil
	}
	for range update.MaxFailedStarts + 1 {
		if err = e.Run(l.Root, "start", out, out); err == nil {
			break
		}
	}
	if now := app.Current(); now != next {
		return err == nil, fmt.Errorf("%s %s failed to start %d times, so it was rolled back to %s and won't be "+
			"offered again", cat.Title, next, update.MaxFailedStarts, now)
	}
	if err != nil {
		return false, fmt.Errorf("%s %s didn't start: %w. See %s status", cat.Title, next, err, cat.Command)
	}
	fmt.Fprintf(out, "%s is running %s\n", cat.Title, next)
	return true, nil
}
