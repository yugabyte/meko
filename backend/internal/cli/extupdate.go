package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/extensions"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/spin"
	"github.com/yugabyte/meko/backend/internal/update"
)

// extUpdateFlags are extension update's: --check, --all, and --auto=on|off, which sets the
// extension's Update automatically and updates nothing.
type extUpdateFlags struct {
	check, all bool
	auto       string
}

// runUpdateExtension is meko extension update <name>|--all [--check] [--auto=on|off]. It needs no
// running server: an extension starts and stops by its own commands. It holds the update lock, so it
// waits for no other update and refuses while one runs.
func runUpdateExtension(l paths.Layout, name string, f extUpdateFlags, stdout, stderr io.Writer) int {
	if f.auto != "" {
		return runSetAuto(l, name, f.auto, stdout, stderr)
	}
	lock := update.NewLock(l.Root)
	if !lock.TryLock() {
		return fail(stderr, update.ErrLocked)
	}
	defer lock.Unlock()
	cfg, err := config.Load(l.Config, os.Getenv, config.Overrides{})
	if err != nil {
		return fail(stderr, err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(stderr, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), extensions.InstallTimeout)
	defer cancel()
	var notices unsignedNotices
	defer notices.print(stdout)
	o := extensions.UpdateOptions{Root: l.Root, Home: home, Client: releaseClient(cfg.Update, notices.add),
		Check: f.check, Out: stdout}
	if !f.all {
		if _, err := extensions.Update(ctx, o, name); err != nil {
			return fail(stderr, err)
		}
		return exitOK
	}
	return updateAll(ctx, l, o, stderr)
}

// updateAll updates every registered extension Meko updates, one after another: one that fails is
// reported and the rest go on, and the exit code says whether any failed. One Meko can't update (a
// build from source) is skipped with a note.
func updateAll(ctx context.Context, l paths.Layout, o extensions.UpdateOptions, stderr io.Writer) int {
	reg, err := extensions.Load(l.Root)
	if err != nil {
		return fail(stderr, err)
	}
	if len(reg.Entries) == 0 {
		fmt.Fprintln(o.Out, "No extensions are installed")
		return exitOK
	}
	looking := spin.Wait(o.Out, "Looking for the extensions' releases")
	o.Releases, err = o.Client.Releases(ctx)
	looking.Clear()
	if err != nil {
		return fail(stderr, err)
	}
	failed := 0
	for _, name := range reg.Names() {
		if cat, ok := extensions.Known[name]; !ok || !cat.Managed {
			continue
		}
		if _, err := extensions.Update(ctx, o, name); err != nil {
			if errors.Is(err, extensions.ErrNotUpdatable) {
				fmt.Fprintf(o.Out, "Skipping %s: %v\n", name, err)
				continue
			}
			fmt.Fprintf(stderr, "meko: %s: %v\n", name, err)
			failed++
		}
	}
	if failed > 0 {
		return fail(stderr, fmt.Errorf("%d extension update(s) failed", failed))
	}
	return exitOK
}

// runSetAuto sets an extension's Update automatically (on or off).
func runSetAuto(l paths.Layout, name, value string, stdout, stderr io.Writer) int {
	var on bool
	switch value {
	case "on":
		on = true
	case "off":
	default:
		return fail(stderr, fmt.Errorf("--auto takes on or off, not %q", value))
	}
	if err := setAutoUpdate(l, name, on); err != nil {
		return fail(stderr, err)
	}
	title := extensions.Known[name].Title
	if on {
		fmt.Fprintf(stdout, "Meko installs %s's updates when it finds them\n", title)
	} else {
		fmt.Fprintf(stdout, "Meko only reports %s's updates now: meko extension update %s installs one\n", title, name)
	}
	return exitOK
}

// setAutoUpdate saves an installed extension's Update automatically in the registry.
func setAutoUpdate(l paths.Layout, name string, on bool) error {
	reg, err := extensions.Load(l.Root)
	if err != nil {
		return err
	}
	e, ok := reg.Entries[name]
	if !ok {
		return fmt.Errorf("%s isn't installed", name)
	}
	e.AutoUpdate = &on
	reg.Entries[name] = e
	return reg.Save()
}
