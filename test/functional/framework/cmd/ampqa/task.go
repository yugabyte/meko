package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/yugabyte/yb-amp/test/functional/framework/core"
	"github.com/yugabyte/yb-amp/test/functional/framework/task"

	// Blank-imported so their init() registers the built-in tasks.
	_ "github.com/yugabyte/yb-amp/test/functional/framework/task/tasks"
)

func newTaskCmd() *cobra.Command {
	var (
		configFiles []string
		jsonConfigs []string
		overrides   []string
		noDefaults  bool
	)
	cmd := &cobra.Command{
		Use:   "task [name]",
		Short: "Run one Task, or list built-ins with no argument",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 0 {
				return listTasks(c)
			}
			name := args[0]
			t, ok := task.Default().Get(name)
			if !ok {
				return &exitError{code: 3, err: fmt.Errorf("task %q not found", name)}
			}
			loadConfig := core.LoadConfig
			if noDefaults {
				loadConfig = core.LoadConfigNoDefaults
			}
			cfg, err := loadConfig(configFiles, jsonConfigs, overrides)
			if err != nil {
				return &exitError{code: 2, err: err}
			}
			deps := task.Deps{
				Config: cfg,
				Seed:   core.NewRunSeed(0, false).ForTest(name),
				Logf:   func(format string, args ...any) { fmt.Fprintf(c.ErrOrStderr(), format+"\n", args...) },
			}
			runErr := t.Run(c.Context(), deps)
			if jsonOutput {
				data := map[string]any{"name": name, "ok": runErr == nil}
				if runErr != nil {
					data["error"] = runErr.Error()
				}
				_ = printEnvelope(c.OutOrStdout(), "ampqa.task", "task "+name, data)
			}
			if runErr != nil {
				return &exitError{code: 1, err: runErr}
			}
			if !jsonOutput {
				fmt.Fprintf(c.OutOrStdout(), "task %s: ok\n", name)
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVarP(&configFiles, "config", "c", nil, "YAML/HOCON config file, deep-merged in order (repeatable)")
	cmd.Flags().StringArrayVar(&jsonConfigs, "jc", nil, "JSON config file, merged after -c (repeatable)")
	cmd.Flags().StringArrayVarP(&overrides, "option", "o", nil, "key=value override with typed prefixes (repeatable)")
	cmd.Flags().BoolVar(&noDefaults, "no-default-config", false, "skip the embedded configs/default.yaml base layer (debugging only)")
	return cmd
}

func listTasks(c *cobra.Command) error {
	tasks := task.Default().List()
	if jsonOutput {
		rows := make([]map[string]any, 0, len(tasks))
		for _, t := range tasks {
			rows = append(rows, map[string]any{"name": t.Name, "doc": t.Doc, "params": t.Params})
		}
		return printEnvelope(c.OutOrStdout(), "ampqa.task.list", "task", map[string]any{"count": len(tasks), "tasks": rows})
	}
	out := c.OutOrStdout()
	for _, t := range tasks {
		fmt.Fprintf(out, "%-18s %s\n", t.Name, t.Doc)
	}
	fmt.Fprintf(out, "%d task(s)\n", len(tasks))
	return nil
}
