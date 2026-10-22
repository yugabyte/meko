package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/store"
	"github.com/yugabyte/meko/backend/internal/update"
)

// runUpdate installs the newest release now, or with check only says whether there is one. A
// running server is restarted into it: by its service, or as start would.
func runUpdate(l paths.Layout, check bool, stdout, stderr io.Writer) int {
	app, ver, ok := runningRelease()
	if !ok {
		return fail(stderr, errors.New("this meko wasn't installed from a release (its binary isn't in an app/<version> "+
			"folder), so it can't update itself: rebuild it, or install a release"))
	}
	cfg, err := config.Load(l.Config, os.Getenv, config.Overrides{})
	if err != nil {
		return fail(stderr, err)
	}
	schema, err := store.LatestSchema()
	if err != nil {
		return fail(stderr, err)
	}
	u := &update.Updater{App: app, Version: ver, Client: newReleaseClient(cfg.Update.Repo), DataDir: l.Root,
		Schema: schema, Log: slog.New(slog.DiscardHandler)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	rel, next, newer, err := u.Check(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	if !newer {
		fmt.Fprintf(stdout, "meko %s is the newest release\n", ver)
		return exitOK
	}
	if check {
		fmt.Fprintf(stdout, "meko %s is available (this is %s): meko update installs it\n", next, ver)
		return exitOK
	}
	fmt.Fprintf(stdout, "Installing meko %s\n", next)
	if err := u.Apply(ctx, rel, next); err != nil {
		return fail(stderr, err)
	}
	if up, _ := running(l.Lock); !up {
		fmt.Fprintf(stdout, "Installed meko %s; it runs at the next meko start\n", next)
		return exitOK
	}
	pid := currentPID(l)
	if m := serviceOf(l); m != nil {
		if err := m.Restart(); err != nil {
			return fail(stderr, err)
		}
	} else {
		if code := stopMeko(l, stdout, stderr); code != exitOK {
			return code
		}
		if code := startDetached(l, config.Overrides{}, app.CurrentExe(), stdout, stderr); code != exitOK {
			return code
		}
	}
	if _, ok := waitAnswering(l, serviceStartTimeout, pid); !ok {
		if serviceOf(l) != nil {
			return fail(stderr, fmt.Errorf("meko %s didn't answer within %s: see meko logs. The service rolls it "+
				"back after %d failed starts", next, serviceStartTimeout, update.MaxFailedStarts))
		}
		return fail(stderr, fmt.Errorf("meko %s didn't answer within %s: see meko logs. Without the service "+
			"nothing starts it again: run meko start, and after %d failed starts the next one rolls back to %s",
			next, serviceStartTimeout, update.MaxFailedStarts, ver))
	}
	fmt.Fprintf(stdout, "Meko is running %s\n", next)
	return exitOK
}
