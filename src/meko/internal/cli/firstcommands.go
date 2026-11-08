package cli

import (
	"context"
	"io"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/callhome"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/spin"
	"github.com/yugabyte/meko/src/meko/internal/update"
	"github.com/yugabyte/meko/src/meko/internal/version"
)

// envParent is set on the meko commands meko runs itself (meko update's extension update --all,
// a staged release's version --json): they aren't commands anyone ran, so they aren't among the
// first commands.
const envParent = update.EnvParent

// tracked is what a run says about itself for the first commands after a first install
// (callhome.Command): filled in as runProg parses, and recorded by Main once the run ends.
type tracked struct {
	cmd     string   // its name in the help (or help, or callhome.UnknownCommand)
	flags   []string // the names of the flags it was given
	root    string   // the data folder it used, once resolved
	skip    bool     // serve and _launch, which no one runs by hand
	started bool     // this run (a first install) began the list, so isn't one of its commands
	leaving *leaving // uninstall's and destroy's event, gathered before they remove anything
}

// stdoutIsTerminal says whether a run's output goes to a terminal (callhome.Command.TTY). Tests
// replace it.
var stdoutIsTerminal = spin.IsTerminal

// finish records the run among the first commands, if the data folder keeps a list with room, and
// sends uninstall's or destroy's event when it worked. It never changes what the run printed or
// its exit code.
func (t *tracked) finish(code int, begun time.Time, stdout io.Writer) {
	if t.skip || t.started || t.cmd == "" || os.Getenv(envParent) != "" {
		return
	}
	c := callhome.Command{Cmd: t.cmd, Flags: t.flags, TTY: stdoutIsTerminal(stdout), Exit: code,
		Ms: time.Since(begun).Milliseconds(), Version: version.Version}
	root := t.root
	if root == "" {
		root, _ = paths.Resolve("")
	}
	if root != "" {
		_ = callhome.RecordCommand(paths.New(root).FirstCommands, c, begun)
	}
	if t.leaving != nil && code == exitOK {
		t.leaving.send(c, begun)
	}
}

// knownCommand says whether name is one of meko's or meko-cli's commands, reported by name.
func knownCommand(name string) bool {
	if name == "help" {
		return true
	}
	_, ok := lookup(progMeko, name)
	if !ok {
		_, ok = lookup(progMekoCLI, name)
	}
	return ok
}

// commandNames are every name knownCommand takes, for serve's usage stats.
func commandNames() []string {
	names := []string{"help"}
	for _, prog := range []string{progMeko, progMekoCLI} {
		for _, c := range commandsOf(prog) {
			names = append(names, c.name)
		}
	}
	return names
}

// leaving is uninstall's or destroy's event: where it goes, and what it says, read before the
// command removes the data folder or the service.
type leaving struct {
	to      callHomeTo
	command string
	payload callhome.Uninstall
	list    callhome.FirstCommands
}

// prepareLeaving gathers uninstall's or destroy's event (command is callhome.UninstallCommand or
// DestroyCommand), or nil when usage stats are off or have nowhere to go.
func prepareLeaving(l paths.Layout, command string, u callhome.Uninstall) *leaving {
	_, _, release := runningRelease()
	to, ok := callHomeTarget(l, release)
	if !ok {
		return nil
	}
	u.Version, u.Platform, u.Arch, u.Service, u.FirstDay = version.Version, runtime.GOOS, runtime.GOARCH, "none",
		to.firstDay
	if m := serviceOf(l); m != nil {
		u.Service = m.Kind()
	}
	list, _ := callhome.ReadFirstCommands(l.FirstCommands, knownCommand)
	return &leaving{to: to, command: command, payload: u, list: list}
}

// send posts the event with the first commands, c last while there's room, waiting at most
// callhome.FailureTimeout and saying nothing.
func (lv *leaving) send(c callhome.Command, begun time.Time) {
	u := lv.payload
	u.FirstCommands = lv.list.Commands
	if !lv.list.Started.IsZero() {
		if next, ok := lv.list.Next(c, begun); ok {
			u.FirstCommands = append(u.FirstCommands, next)
		}
	}
	_ = callhome.SendUninstall(context.Background(), http.DefaultClient, lv.to.url, lv.command, lv.to.installID,
		u, time.Now())
}
