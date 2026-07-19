// Command yb_amp_local is the Go port of the Rust `yb_amp_local` launcher: it
// creates and drives a local yb-amp environment (controller + pageservers +
// safekeepers + endpoint_storage + computes) for integration testing. It is a
// drop-in replacement for the Rust binary, exposing the same subcommands and
// flags the integration harness depends on.
//
// `init` materializes the `.yb_amp/` repo and runs without the repo lock;
// every other command takes an flock on the repo dir first, loads the config,
// runs, and re-persists the config only if it changed. A `start` failure exits
// with code 2 (after a best-effort stop-all); any other error prints
// "command failed: <e>" and exits 1.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yugabyte/amp/controller/local/config"
	"github.com/yugabyte/amp/controller/local/launch"
	"github.com/yugabyte/amp/controller/local/proc"
)

func main() {
	root := &cobra.Command{
		Use:           "yb_amp_local",
		Short:         "Manage a local yb-amp environment for testing",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		initCmd(),
		startCmd(),
		branchCmd(),
		endpointCmd(),
		pageserverCmd(),
		safekeeperCmd(),
		endpointStorageCmd(),
		controllerCmd(),
	)

	if err := root.Execute(); err != nil {
		var sf *launch.StartFailure
		if errors.As(err, &sf) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "command failed: %v\n", err)
		os.Exit(1)
	}
}

// withEnv is the shared wrapper for every non-`init` command: it takes the
// repo lock, loads the config, runs the handler, and re-persists the config
// only when the handler mutated its persisted-mutable state. Mirrors the
// lock + load + `original_env != env` persist guard in Rust `main`.
func withEnv(run func(context.Context, *config.LocalEnv) error) error {
	base, err := config.BasePath()
	if err != nil {
		return err
	}
	lock, err := proc.LockRepo(base)
	if err != nil {
		return err
	}
	defer lock.Close()

	env, err := config.LoadConfig(base)
	if err != nil {
		return fmt.Errorf("Error loading config: %w", err)
	}
	before := launch.SnapshotMutable(env)
	if err := run(context.Background(), env); err != nil {
		return err
	}
	if launch.MutableChanged(before, env) {
		return env.PersistConfig()
	}
	return nil
}

// --- init ---

func initCmd() *cobra.Command {
	var args launch.InitArgs
	var force string
	var portBase uint16
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a new yb-amp repository",
		RunE: func(cmd *cobra.Command, _ []string) error {
			args.Force = launch.InitForceMode(force)
			if cmd.Flags().Changed("port-base") {
				args.PortBase = &portBase
			} else {
				args.PortBase = nil
			}
			return launch.Init(args)
		},
	}
	f := cmd.Flags()
	f.StringVar(&args.YbDbname, "yb-dbname", "", "YugabyteDB database name")
	f.StringVar(&args.YbHost, "yb-host", "", "YugabyteDB host")
	f.Uint16Var(&args.YbPort, "yb-port", 0, "YugabyteDB port")
	f.StringVar(&args.YbHotPath, "yb-hot-path", "tsclient", "hot-path transport: tsclient or ysql")
	f.Uint16Var(&args.NumPageservers, "num-pageservers", 1, "how many pageservers to create")
	f.StringVar(&force, "force", string(launch.ForceMustNotExist), "force mode: must-not-exist | empty-dir-ok | remove-all-contents")
	f.Uint16Var(&portBase, "port-base", 0, "relocate the cluster into a free port block starting at this base")
	return cmd
}

// --- start ---

func startCmd() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the controller, pageservers and safekeepers",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withEnv(func(ctx context.Context, env *config.LocalEnv) error {
				return launch.StartAll(ctx, env, timeout)
			})
		},
	}
	cmd.Flags().DurationVar(&timeout, "start-timeout", 120*time.Second, "start timeout")
	return cmd
}

// --- branch ---

func branchCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "branch", Short: "Manage branches"}
	cmd.AddCommand(branchCreateCmd(), branchDropCmd())
	return cmd
}

func branchDropCmd() *cobra.Command {
	var branchID string
	cmd := &cobra.Command{
		Use:   "drop",
		Short: "Drop a branch (stop+destroy its endpoints, mark it dropped)",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withEnv(func(ctx context.Context, env *config.LocalEnv) error {
				return launch.BranchDrop(ctx, env, branchID)
			})
		},
	}
	cmd.Flags().StringVar(&branchID, "branch-id", "", "branch id (required)")
	_ = cmd.MarkFlagRequired("branch-id")
	return cmd
}

func branchCreateCmd() *cobra.Command {
	var a launch.BranchCreateArgs
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a root branch (no ancestor) or a fork",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withEnv(func(ctx context.Context, env *config.LocalEnv) error {
				return launch.BranchCreate(ctx, env, a)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&a.BranchID, "branch-id", "", "new branch id (generated if omitted)")
	f.StringVar(&a.BranchName, "branch-name", "", "human-readable branch alias")
	f.StringVar(&a.AncestorBranchName, "ancestor-branch-name", "", "ancestor branch to fork from")
	f.StringVar(&a.AncestorStartLSN, "ancestor-start-lsn", "", "specific ancestor LSN to fork at")
	f.IntVar(&a.PgVersion, "pg-version", config.DefaultPgVersion, "Postgres version for a root branch")
	f.BoolVar(&a.SetDefault, "set-default", false, "set this branch as the default (root only)")
	return cmd
}

// --- endpoint ---

func endpointCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "endpoint", Short: "Manage compute endpoints"}
	cmd.AddCommand(endpointCreateCmd(), endpointStartCmd(), endpointStopCmd())
	return cmd
}

func endpointCreateCmd() *cobra.Command {
	var a launch.EndpointCreateArgs
	var pgPort, extPort, intPort uint16
	var pgSettings []string
	cmd := &cobra.Command{
		Use:   "create [endpoint_id]",
		Short: "Create a compute endpoint (provisions the branch's PS+SK pair)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, posArgs []string) error {
			if len(posArgs) == 1 {
				a.EndpointID = posArgs[0]
			}
			if cmd.Flags().Changed("pg-port") {
				a.PgPort = &pgPort
			}
			if cmd.Flags().Changed("external-http-port") {
				a.ExternalHTTPPort = &extPort
			}
			if cmd.Flags().Changed("internal-http-port") {
				a.InternalHTTPPort = &intPort
			}
			for _, s := range pgSettings {
				k, v, ok := strings.Cut(s, "=")
				if !ok {
					return fmt.Errorf("invalid --pg-setting %q, expected NAME=VALUE", s)
				}
				a.PgSettings = append(a.PgSettings, [2]string{k, v})
			}
			return withEnv(func(ctx context.Context, env *config.LocalEnv) error {
				return launch.EndpointCreate(ctx, env, a)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&a.BranchID, "branch-id", "", "branch id")
	f.StringVar(&a.BranchName, "branch-name", "", "branch name")
	f.IntVar(&a.PgVersion, "pg-version", config.DefaultPgVersion, "Postgres version")
	f.Uint16Var(&pgPort, "pg-port", 0, "compute Postgres port")
	f.Uint16Var(&extPort, "external-http-port", 0, "compute external HTTP port")
	f.Uint16Var(&intPort, "internal-http-port", 0, "compute internal HTTP port")
	f.StringVar(&a.LSN, "lsn", "", "start LSN (static mode; unsupported)")
	f.BoolVar(&a.ConfigOnly, "config-only", false, "create config files only (accepted for compat)")
	f.StringArrayVar(&pgSettings, "pg-setting", nil, "repeatable NAME=VALUE Postgres setting")
	return cmd
}

func endpointStartCmd() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "start <endpoint_id>",
		Short: "Start a compute endpoint",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, posArgs []string) error {
			return withEnv(func(ctx context.Context, env *config.LocalEnv) error {
				return launch.EndpointStart(ctx, env, posArgs[0], timeout)
			})
		},
	}
	cmd.Flags().DurationVarP(&timeout, "start-timeout", "t", 180*time.Second, "start timeout")
	return cmd
}

func endpointStopCmd() *cobra.Command {
	var mode string
	var destroy bool
	cmd := &cobra.Command{
		Use:   "stop <endpoint_id>",
		Short: "Stop a compute endpoint",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, posArgs []string) error {
			return withEnv(func(ctx context.Context, env *config.LocalEnv) error {
				return launch.EndpointStop(ctx, env, posArgs[0], launch.EndpointStopMode(mode), destroy)
			})
		},
	}
	cmd.Flags().StringVar(&mode, "mode", "fast", "stop mode: fast | immediate | immediate-terminate")
	cmd.Flags().BoolVar(&destroy, "destroy", false, "also deprovision the co-located pageserver/safekeeper/endpoint_storage and remove the endpoint")
	return cmd
}

// --- pageserver ---

func pageserverCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "pageserver", Short: "Manage pageservers"}
	cmd.AddCommand(pageserverStartCmd(), pageserverStopCmd(), pageserverRestartCmd())
	return cmd
}

func pageserverStartCmd() *cobra.Command {
	var id int64
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start a pageserver",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withEnv(func(ctx context.Context, env *config.LocalEnv) error {
				return launch.PageserverStart(ctx, env, id, timeout)
			})
		},
	}
	cmd.Flags().Int64Var(&id, "id", 1, "pageserver id")
	cmd.Flags().DurationVarP(&timeout, "start-timeout", "t", 10*time.Second, "start timeout")
	return cmd
}

func pageserverStopCmd() *cobra.Command {
	var id int64
	var mode string
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop a pageserver",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withEnv(func(_ context.Context, env *config.LocalEnv) error {
				return launch.PageserverStop(env, id, mode == "immediate")
			})
		},
	}
	cmd.Flags().Int64Var(&id, "id", 1, "pageserver id")
	cmd.Flags().StringVarP(&mode, "mode", "m", "fast", "stop mode: fast | immediate")
	return cmd
}

func pageserverRestartCmd() *cobra.Command {
	var id int64
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Restart a pageserver",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withEnv(func(ctx context.Context, env *config.LocalEnv) error {
				return launch.PageserverRestart(ctx, env, id, timeout)
			})
		},
	}
	cmd.Flags().Int64Var(&id, "id", 1, "pageserver id")
	cmd.Flags().DurationVarP(&timeout, "start-timeout", "t", 10*time.Second, "start timeout")
	return cmd
}

// --- safekeeper ---

func safekeeperCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "safekeeper", Short: "Manage safekeepers"}
	cmd.AddCommand(safekeeperStartCmd(), safekeeperStopCmd(), safekeeperRestartCmd())
	return cmd
}

func safekeeperStopCmd() *cobra.Command {
	var mode string
	cmd := &cobra.Command{
		Use:   "stop [id]",
		Short: "Stop a safekeeper",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, posArgs []string) error {
			return withEnv(func(_ context.Context, env *config.LocalEnv) error {
				return launch.SafekeeperStop(env, safekeeperID(posArgs), mode == "immediate")
			})
		},
	}
	cmd.Flags().StringVarP(&mode, "mode", "m", "fast", "stop mode: fast | immediate")
	return cmd
}

func safekeeperRestartCmd() *cobra.Command {
	var mode string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "restart [id]",
		Short: "Restart a safekeeper",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, posArgs []string) error {
			return withEnv(func(_ context.Context, env *config.LocalEnv) error {
				return launch.SafekeeperRestart(env, safekeeperID(posArgs), mode == "immediate", timeout)
			})
		},
	}
	cmd.Flags().StringVarP(&mode, "mode", "m", "fast", "stop mode: fast | immediate")
	cmd.Flags().DurationVarP(&timeout, "start-timeout", "t", 10*time.Second, "start timeout")
	return cmd
}

// safekeeperID parses the optional positional safekeeper id (default 1).
func safekeeperID(posArgs []string) config.NodeID {
	if len(posArgs) == 1 {
		if v, err := strconv.ParseInt(posArgs[0], 10, 64); err == nil {
			return config.NodeID(v)
		}
	}
	return 1
}

func safekeeperStartCmd() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "start [id]",
		Short: "Start a safekeeper",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, posArgs []string) error {
			id := int64(1)
			if len(posArgs) == 1 {
				parsed, err := parseInt64(posArgs[0])
				if err != nil {
					return fmt.Errorf("invalid safekeeper id %q: %w", posArgs[0], err)
				}
				id = parsed
			}
			return withEnv(func(_ context.Context, env *config.LocalEnv) error {
				return launch.SafekeeperStart(env, id, timeout)
			})
		},
	}
	cmd.Flags().DurationVarP(&timeout, "start-timeout", "t", 10*time.Second, "start timeout")
	return cmd
}

// --- endpoint-storage ---

func endpointStorageCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "endpoint-storage", Short: "Manage object storage"}
	cmd.AddCommand(endpointStorageStartCmd())
	return cmd
}

func endpointStorageStartCmd() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start all provisioned endpoint_storage instances",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withEnv(func(_ context.Context, env *config.LocalEnv) error {
				return launch.EndpointStorageStart(env, timeout)
			})
		},
	}
	cmd.Flags().DurationVarP(&timeout, "start-timeout", "t", 10*time.Second, "start timeout")
	return cmd
}

// --- controller ---

func controllerCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "controller", Short: "Manage the controller"}
	cmd.AddCommand(controllerStartCmd(), controllerStopCmd())
	return cmd
}

func controllerStartCmd() *cobra.Command {
	var timeout time.Duration
	var instanceID uint8
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the controller",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withEnv(func(ctx context.Context, env *config.LocalEnv) error {
				return launch.ControllerStart(ctx, env, instanceID, timeout)
			})
		},
	}
	cmd.Flags().DurationVarP(&timeout, "start-timeout", "t", 10*time.Second, "start timeout")
	cmd.Flags().Uint8Var(&instanceID, "instance-id", 1, "controller instance id")
	return cmd
}

func controllerStopCmd() *cobra.Command {
	var mode string
	var instanceID uint8
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the controller",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withEnv(func(_ context.Context, env *config.LocalEnv) error {
				return launch.ControllerStop(env, instanceID, mode == "immediate")
			})
		},
	}
	cmd.Flags().StringVarP(&mode, "mode", "m", "fast", "stop mode: fast | immediate")
	cmd.Flags().Uint8Var(&instanceID, "instance-id", 1, "controller instance id")
	return cmd
}

func parseInt64(s string) (int64, error) {
	var n int64
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int64(r-'0')
	}
	return n, nil
}
