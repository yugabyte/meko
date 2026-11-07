package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/yugabyte/meko/src/meko/internal/extensions"
	"github.com/yugabyte/meko/src/meko/internal/paths"
)

// extensionOfVerb is the extension whose commands meko <verb> runs (Catalog.Verb): kode, AgentK's;
// present, Presenter's.
func extensionOfVerb(verb string) (string, bool) {
	for name, cat := range extensions.Known {
		if cat.Verb != "" && cat.Verb == verb {
			return name, true
		}
	}
	return "", false
}

// runExtensionCommand runs an installed extension's own command, meko <verb> <args>: its registered
// program with args, MEKO_HOME naming the data folder, on this terminal, and exits as it does. A
// leading --data-dir is Meko's (neither extension has one); everything else is the extension's,
// its --help too.
func runExtensionCommand(name string, args []string, stdout, stderr io.Writer) int {
	dataDir, args := leadingDataDir(args)
	root, err := paths.Resolve(dataDir)
	if err != nil {
		return fail(stderr, err)
	}
	cat := extensions.Known[name]
	reg, err := extensions.Load(root)
	if err != nil {
		return fail(stderr, err)
	}
	e, ok := reg.Entries[name]
	if !ok {
		return fail(stderr, fmt.Errorf("%s isn't installed: add it with meko extension install %s", cat.Title, name))
	}
	if !executableFile(e.Command) {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return fail(stderr, herr)
		}
		if e.Command = reg.FindAgain(name, home, root); e.Command == "" {
			return fail(stderr, fmt.Errorf("%s %s", cat.Title, extensions.Damaged(name, reg.Entries[name].Command)))
		}
	}
	return runProgram(e.Command, args, extensions.CommandEnv(root, e.Command), stdout, stderr)
}

// leadingDataDir takes --data-dir <dir> or --data-dir=<dir> off the front of args.
func leadingDataDir(args []string) (string, []string) {
	switch {
	case len(args) > 1 && (args[0] == "--data-dir" || args[0] == "-data-dir"):
		return args[1], args[2:]
	case len(args) > 0 && strings.HasPrefix(args[0], "--data-dir="):
		return strings.TrimPrefix(args[0], "--data-dir="), args[1:]
	}
	return "", args
}

// runProgram runs program with args and env on this terminal (stdin, and stdout and stderr), and
// returns its exit code: 128 plus the signal's number when a signal ended it. Ctrl+C and the like
// reach it from the terminal, so meko only waits for it to end; a SIGTERM or SIGHUP sent to meko
// alone is passed on.
func runProgram(program string, args, env []string, stdout, stderr io.Writer) int {
	cmd := exec.Command(program, args...) //nolint:gosec // G204: an installed extension's own program
	cmd.Stdin, cmd.Stdout, cmd.Stderr, cmd.Env = stdin, stdout, stderr, env
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		return fail(stderr, err)
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				if s == syscall.SIGTERM || s == syscall.SIGHUP {
					_ = cmd.Process.Signal(s)
				}
			case <-done:
				return
			}
		}
	}()
	err := cmd.Wait()
	close(done)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// executableFile reports whether p is a file someone may run.
func executableFile(p string) bool {
	fi, err := os.Stat(p) //nolint:gosec // G703: an extension's program, from the registry Meko keeps
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}
