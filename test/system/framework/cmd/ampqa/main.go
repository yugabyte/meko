// Command ampqa is the yb-amp QA framework binary. It wires the CLI surface
// (run/setup/collect/describe/manifest/version/env/task/report) over the core
// engine, the environment providers, and the reporting sinks.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	// Blank-imported so their init() registers the built-in tests.
	_ "github.com/yugabyte/yb-amp/test/system/suites/canary"
	_ "github.com/yugabyte/yb-amp/test/system/suites/features"
	_ "github.com/yugabyte/yb-amp/test/system/suites/perf"
	_ "github.com/yugabyte/yb-amp/test/system/suites/presets"
	_ "github.com/yugabyte/yb-amp/test/system/suites/stress"
)

const version = "0.0.0-m0"

var jsonOutput bool

// exitError carries a process exit code. A nil err means "exit with this code,
// print nothing" (used for test-failure exit 1).
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit %d", e.code)
	}
	return e.err.Error()
}

func (e *exitError) Unwrap() error { return e.err }

type envelope struct {
	Kind      string `json:"kind"`
	Version   int    `json:"version"`
	Command   string `json:"command"`
	Data      any    `json:"data"`
	Truncated bool   `json:"truncated"`
}

func printEnvelope(w io.Writer, kind, command string, data any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(envelope{Kind: kind, Version: 1, Command: command, Data: data, Truncated: false})
}

func buildRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "ampqa",
		Short:         "yb-amp QA framework",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVar(&jsonOutput, "json", false, "emit the JSON envelope on stdout")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &exitError{code: 2, err: err}
	})
	root.AddCommand(newRunCmd(), newSetupCmd(), newCollectCmd(), newDescribeCmd(), newManifestCmd(), newVersionCmd(), newEnvCmd(), newTaskCmd(), newReportCmd())
	return root
}

func main() {
	// The root context is cancelled on the first SIGINT/SIGTERM so an in-flight
	// run/setup stops taking new work and runs its teardown/reap path before the
	// process exits, instead of dying and orphaning provisioned infrastructure
	// (cloud VMs, k8s namespaces, kubectl port-forward subprocesses). A SECOND
	// signal restores the default behavior (immediate hard exit) as the escape
	// hatch when a graceful teardown is itself hanging.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := buildRoot()
	err := root.ExecuteContext(ctx)
	if err == nil {
		return
	}
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.err != nil {
			fmt.Fprintln(os.Stderr, "error:", ee.err)
		}
		os.Exit(ee.code)
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
