package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/meko/src/meko/internal/callhome"
	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/extensions"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/server"
	"github.com/yugabyte/meko/src/meko/internal/service"
	"github.com/yugabyte/meko/src/meko/internal/spin"
	"github.com/yugabyte/meko/src/meko/internal/store"
)

// bugForm is where a failure install can't explain is reported.
const bugForm = "https://github.com/yugabyte/meko/issues/new?template=bug.yml"

// installing is what meko install knows while it runs, for ending a failed run the one way
// (installing.fail): its report, the version it installs and the one it replaces, the service
// manager's kind, and whether it has written anything yet.
type installing struct {
	l       paths.Layout
	r       report
	run     installRunInfo
	release bool   // a release install, which reports failures to call home
	version string // the version being set up
	from    string // the version an upgrade replaces
	service string // the service manager's kind, or none
	written bool   // app/current, the data folder or the service were written
}

// installFailure is one way install failed: the row's label, call home's step and error class
// (callhome.Step*, callhome.Kind*), the error, whether its row is printed already, and the closing
// line when it isn't the usual one.
type installFailure struct {
	label, step, kind string
	err               error
	printed           bool
	closing           string
}

// fail ends a failed install: a ✗ row for the step (unless the step printed its own), a line
// saying whether running the installer again is safe or finishes the setup, a link to report a
// bug for a failure that isn't one of the known ones, and the failure's step and class to call
// home. The error goes on the row, not again on stderr.
func (in *installing) fail(f installFailure) int {
	if !f.printed {
		in.r.step(stepFail, f.label, f.err.Error())
	}
	closing := f.closing
	switch {
	case closing != "":
	case !in.written:
		closing = "Nothing was left half-done: running the installer again is safe"
	case in.run.started.IsZero():
		closing = "Fix it, then run meko install again to finish the setup"
	default:
		closing = "Fix it, then run the installer again to finish the setup"
	}
	fmt.Fprintf(in.r.w, "  %s\n", spin.Dim(closing, in.r.color))
	if !knownFailure(f.kind) {
		fmt.Fprintf(in.r.w, "  %s%s\n", spin.Dim("Report a bug: ", in.r.color), spin.Link(bugForm, in.r.color))
	}
	sendInstallFailure(in.l, in.release, callhome.Failure{Step: f.step, Kind: f.kind, Version: in.version,
		From: in.from, First: in.run.first, Platform: runtime.GOOS, Arch: runtime.GOARCH, Service: in.service})
	return exitError
}

// knownFailure reports whether a failure's class is one install explains with its fix, so it
// needs no bug report.
func knownFailure(kind string) bool {
	switch kind {
	case callhome.KindServiceManager, callhome.KindPortInUse, callhome.KindNoFreePort:
		return true
	}
	return false
}

// sendInstallFailure sends a failed install's step and error class to call home, where the usage
// stats would go: from a release install or to [call_home] url, unless config.toml, the
// environment or the web UI's setting (read from the database, which no server may be running
// to answer for) turned them off, or the database is there but can't be read. It waits at most
// callhome.FailureTimeout and says nothing.
var sendInstallFailure = func(l paths.Layout, release bool, f callhome.Failure) {
	cfg, err := config.Load(l.Config, os.Getenv, config.Overrides{})
	if err != nil || !cfg.CallHome.Enabled {
		return
	}
	url := cfg.CallHome.URL
	if url == "" && release {
		url = callhome.DefaultURL
	}
	if url == "" {
		return
	}
	ctx := context.Background()
	_, settings, err := store.Peek(ctx, l.DB, store.KeyInstallID, callhome.KeyEnabled)
	if err != nil || settings[callhome.KeyEnabled] == "false" {
		return // a database that can't be read can't say the web UI didn't turn usage stats off
	}
	id := settings[store.KeyInstallID]
	if id == "" {
		id = uuid.NewString() // no database yet: an id for this report alone
	}
	_ = callhome.SendFailure(ctx, http.DefaultClient, url, id, f, time.Now())
}

// portScan is how many ports after Meko's install tries when another program holds it.
const portScan = 20

// pickPort makes sure Meko's port is free before anything else is written: the server would
// otherwise fail to start once the service is. A port a Meko server that install replaces holds
// is fine (mekoHolds). Another program's is the user's to sort out when they chose the port
// (config.PortChosen), or when a Kode is on this computer: one from before Meko served it holds
// the default port until it's updated, and Meko should then take that port over, not move off it.
// Otherwise install takes the first free port of the portScan after it, writes it to config.toml
// (config.SetPickedPort) and says so; agents connected to the old port follow at the server's start
// (followPort). A port install picked goes back to the default once that's free again
// (config.ClearPickedPort). m is nil with --no-service.
func pickPort(l paths.Layout, m service.Manager, home string, r report) *installFailure {
	cfg, err := config.Load(l.Config, os.Getenv, config.Overrides{})
	if err != nil {
		return &installFailure{label: "Server", step: callhome.StepChecks, kind: callhome.KindError, err: err}
	}
	port := cfg.Server.Port
	cfgPath := homeRel(l.Config, home)
	chosen, err := config.PortChosen(l.Config, os.Getenv)
	if err != nil {
		return &installFailure{label: "Server", step: callhome.StepChecks, kind: callhome.KindError, err: err}
	}
	defaultFree := mekoHolds(l, m, config.DefaultPort) || portFree(config.DefaultPort) == nil
	if !chosen && port != config.DefaultPort && defaultFree {
		if err = config.ClearPickedPort(l.Config); err != nil {
			return &installFailure{label: "Port", step: callhome.StepChecks, kind: callhome.KindError, err: err}
		}
		r.step(stepOK, "Port", fmt.Sprintf("back on %d", config.DefaultPort)+
			spin.Dim(fmt.Sprintf(" · %d, which install picked, is out of %s", port, cfgPath), r.color))
		return nil
	}
	if mekoHolds(l, m, port) || portFree(port) == nil {
		return nil
	}
	inUse := fmt.Sprintf("port %d is in use by another program (a Kode from before Meko served it, or another)", port)
	switch {
	case os.Getenv("MEKO_PORT") != "":
		portInUse(r, inUse, port, "change or unset MEKO_PORT")
		return &installFailure{step: callhome.StepChecks, kind: callhome.KindPortInUse, printed: true}
	case chosen || extensions.Known["kode"].Find("kode", home, l.Root) != "":
		portInUse(r, inUse, port, "set [server] port in "+cfgPath)
		return &installFailure{step: callhome.StepChecks, kind: callhome.KindPortInUse, printed: true}
	}
	for p := port + 1; p <= port+portScan && p <= 65535; p++ {
		if portFree(p) != nil {
			continue
		}
		if err = l.Ensure(); err == nil {
			err = config.SetPickedPort(l.Config, p)
		}
		if err != nil {
			return &installFailure{label: "Port", step: callhome.StepChecks, kind: callhome.KindError,
				err: fmt.Errorf("port %d is in use, and port %d can't be set in %s: %w", port, p, cfgPath, err)}
		}
		r.step(stepWarn, "Port", fmt.Sprintf("%d is in use by another program", port)+
			spin.Dim(fmt.Sprintf(" · Meko uses %d, set in %s", p, cfgPath), r.color))
		return nil
	}
	portInUse(r, fmt.Sprintf("ports %d to %d are all in use by other programs", port, port+portScan), port,
		"set [server] port in "+cfgPath)
	return &installFailure{step: callhome.StepChecks, kind: callhome.KindNoFreePort, printed: true}
}

// startWait is how the wait for a new server ended: its status when one answered, and what the
// service's stderr (service.log) gained meanwhile: the last error line serve printed and the
// last few lines.
type startWait struct {
	st      server.StatusReply
	ok      bool
	errLine string
	tail    []string
}

// startErrors is how many errors serve prints to service.log, each from a start of its own, before
// install stops waiting: the service starts it again every few seconds, and twice the same is no
// passing hiccup.
const startErrors = 2

// tailLines is how many of service.log's new lines a failed start shows.
const tailLines = 5

// waitStarted waits, at most d, until a server other than notPID answers (waitAnswering). Unless
// it mayn't stop early (an upgrade on probation, which needs its failed starts to roll back), it
// stops once service.log, from offset from, has startErrors of serve's "meko: " lines.
func waitStarted(l paths.Layout, d time.Duration, notPID int, from int64, early bool) startWait {
	var w startWait
	for deadline := time.Now().Add(d); ; time.Sleep(200 * time.Millisecond) {
		w.st, w.ok = answering(l, notPID)
		lines := linesSince(l.ServiceLog, from)
		var errs int
		for _, line := range lines {
			if msg, ok := strings.CutPrefix(line, "meko: "); ok {
				errs++
				w.errLine = msg
			}
		}
		w.tail = lines[max(0, len(lines)-tailLines):]
		if w.ok || (early && errs >= startErrors) || !time.Now().Before(deadline) {
			return w
		}
	}
}

// fileSize is path's size, or 0.
func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// maxLogRead is the most of service.log a failed start reads: its end.
const maxLogRead = 1 << 20

// linesSince is the lines of the file at path after offset from (all of it if it's shorter now, and
// only its last maxLogRead bytes if more came), without the empty ones.
func linesSince(path string, from int64) []string {
	f, err := os.Open(path) //nolint:gosec // G304: the data folder's own log
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	if fi, serr := f.Stat(); serr == nil {
		if fi.Size() < from {
			from = 0 // a new file
		}
		from = max(from, fi.Size()-maxLogRead) // the latest lines, when more than that came
	}
	if _, err = f.Seek(from, io.SeekStart); err != nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(f, maxLogRead))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// startFailed prints the end of a failed start: the service log's last lines, how to see more, and
// the service's own view.
func startFailed(r report, w startWait, m service.Manager, stopped bool) {
	fmt.Fprintln(r.w)
	for _, line := range w.tail {
		fmt.Fprintf(r.w, "    %s\n", spin.Dim(line, r.color))
	}
	fmt.Fprintf(r.w, "    %s%s\n", spin.Dim("More                 ", r.color), spin.Blue("meko logs --service", r.color))
	note := ""
	if stopped {
		note = spin.Dim(" · stopped so it doesn't keep restarting", r.color)
	}
	fmt.Fprintf(r.w, "    %s%s%s\n", spin.Dim("The service          ", r.color), spin.Blue(m.StatusHint(), r.color), note)
}

// startReason is why a start failed, and its class for call home: serve's own error, or that it
// didn't answer within d.
func startReason(w startWait, d time.Duration) (reason, kind string) {
	if w.errLine != "" {
		return "didn't start: " + w.errLine, callhome.KindStartFailed
	}
	return fmt.Sprintf("didn't answer within %s", d), callhome.KindStartTimeout
}
