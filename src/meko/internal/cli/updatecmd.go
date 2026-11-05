package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/extensions"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/spin"
	"github.com/yugabyte/meko/src/meko/internal/store"
	"github.com/yugabyte/meko/src/meko/internal/update"
)

// runUpdate installs Meko's newest release now, then every extension's, or with check only says
// whether there are any. A running server is restarted into the new Meko: by its service, or as
// start would. The extensions are then updated by the new meko (extension update --all), whatever
// their Update automatically says. A build from source skips Meko and updates the extensions.
func runUpdate(l paths.Layout, check bool, stdout, stderr io.Writer) int {
	exe, code := updateMeko(l, check, stdout, stderr)
	if code != exitOK {
		return code
	}
	if exe == "" {
		return runUpdateExtension(l, "", extUpdateFlags{all: true, check: check}, stdout, stderr)
	}
	args := []string{"extension", "update", "--all", "--data-dir", l.Root}
	if check {
		args = append(args, "--check")
	}
	cmd := exec.Command(exe, args...) //nolint:gosec // G204: the meko release just installed
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return fail(stderr, err)
	}
	return exitOK
}

// updateMeko is runUpdate's part for Meko itself. It returns the meko to update the extensions with:
// the release just installed, or "" for this one (nothing newer, only a check, or a build from
// source).
func updateMeko(l paths.Layout, check bool, stdout, stderr io.Writer) (string, int) {
	app, ver, ok := runningRelease()
	if !ok {
		fmt.Fprintln(stdout, "This meko wasn't installed from a release (its binary isn't in an app/<version> folder), "+
			"so it doesn't update itself: rebuild it, or install a release")
		return "", exitOK
	}
	cfg, err := config.Load(l.Config, os.Getenv, config.Overrides{})
	if err != nil {
		return "", fail(stderr, err)
	}
	schema, err := store.LatestSchema()
	if err != nil {
		return "", fail(stderr, err)
	}
	lock := update.NewLock(l.Root)
	if !lock.TryLock() {
		return "", fail(stderr, update.ErrLocked)
	}
	defer lock.Unlock()
	var notices unsignedNotices
	u := &update.Updater{App: app, Version: ver, Client: releaseClient(cfg.Update, notices.add), DataDir: l.Root,
		Schema: schema, Log: slog.New(slog.DiscardHandler), Lock: lock,
		Extensions: func(rels []update.Release) map[string]update.ExtensionState { return extensions.Check(l.Root, rels) }}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	looking := spin.Wait(stdout, "Looking for a newer Meko release")
	rel, next, newer, err := u.Check(ctx)
	looking.Clear()
	if err != nil {
		return "", fail(stderr, err)
	}
	if !newer {
		fmt.Fprintf(stdout, "meko %s is the newest release\n", ver)
		return "", exitOK
	}
	if check {
		fmt.Fprintf(stdout, "meko %s is available (this is %s): meko update installs it\n", next, ver)
		return "", exitOK
	}
	installing := spin.Start(stdout, "Release", "installing meko "+next)
	if err := u.Apply(ctx, rel, next); err != nil {
		installing.Fail()
		return "", fail(stderr, err)
	}
	installing.Done("meko " + next + " installed")
	notices.print(stdout)
	if up, _ := running(l.Lock); !up {
		fmt.Fprintf(stdout, "Installed meko %s; it runs at the next meko start\n", next)
		return app.CurrentExe(), exitOK
	}
	pid := currentPID(l)
	m := serviceOf(l)
	// The service restarts Meko quietly, so its row shows the dots; by hand, stopMeko says it
	// stopped and startDetached prints the Server row once the new Meko answers.
	var restarting *spin.Step
	if m != nil {
		restarting = spin.Start(stdout, "Server", "restarting into meko "+next)
		if err := m.Restart(); err != nil {
			restarting.Fail()
			return "", fail(stderr, err)
		}
	} else {
		if code := stopMeko(l, stdout, stderr); code != exitOK {
			return "", code
		}
		if code := startDetached(l, config.Overrides{}, app.CurrentExe(), stdout, stderr); code != exitOK {
			return "", code
		}
	}
	st, ok := waitAnswering(l, serviceStartTimeout, pid)
	if !ok {
		if m != nil {
			restarting.Fail()
			return "", fail(stderr, fmt.Errorf("meko %s didn't answer within %s: see meko logs. The service rolls it "+
				"back after %d failed starts", next, serviceStartTimeout, update.MaxFailedStarts))
		}
		return "", fail(stderr, fmt.Errorf("meko %s didn't answer within %s: see meko logs. Without the service "+
			"nothing starts it again: run meko start, and after %d failed starts the next one rolls back to %s",
			next, serviceStartTimeout, update.MaxFailedStarts, ver))
	}
	if m != nil {
		restarting.Done(fmt.Sprintf("running at http://127.0.0.1:%d (pid %d)", st.Port, st.PID))
	}
	fmt.Fprintf(stdout, "Meko is running %s\n", next)
	return app.CurrentExe(), exitOK
}
