package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yugabyte/yb-amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/yb-amp/test/functional/framework/apps/cloud"
	"github.com/yugabyte/yb-amp/test/functional/framework/core"
	"github.com/yugabyte/yb-amp/test/functional/framework/env/registry"
)

// cloudVMForReap builds the cloud VM client `env reap` drives. A package var so
// unit tests substitute a fake that records ReapByTag/ReapStale without touching
// a real cloud.
var cloudVMForReap = func(name string, cfg cloud.ConfigGetter, resolve cloud.SecretResolver) (cloud.VM, error) {
	return cloud.New(name, cfg, resolve)
}

func newEnvCmd() *cobra.Command {
	var regPath string
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Standing environment registry (file store until the portal)",
	}
	cmd.PersistentFlags().StringVar(&regPath, "registry", "", "registry file path (default ~/.ampqa/envs.json)")
	cmd.AddCommand(newEnvLsCmd(&regPath), newEnvRmCmd(&regPath), newEnvReapCmd(), newEnvRefreshSecretCmd(&regPath))
	return cmd
}

// newEnvRefreshSecretCmd builds `ampqa env refresh-secret <id>`: re-mint the ECR
// image-pull secret for a registered ampstack (k8s control-plane) standing env
// and re-apply it, then roll the controller so it re-pulls. ECR login tokens
// expire after ~12h, so a long-lived setup's control plane falls into
// ImagePullBackOff once a pod reschedules; this heals it in place.
func newEnvRefreshSecretCmd(regPath *string) *cobra.Command {
	var (
		configFiles []string
		jsonConfigs []string
		overrides   []string
		noDefaults  bool
		noRoll      bool
	)
	cmd := &cobra.Command{
		Use:   "refresh-secret <id>",
		Short: "Re-mint the image-pull secret for a standing k8s env (ECR tokens expire ~12h)",
		Long: `Re-mint the ECR image-pull secret for a registered ampstack (k8s control-plane)
standing environment and re-apply it to the env namespace, then roll the
controller so it re-pulls immediately. ECR login tokens expire after ~12h, so a
long-lived setup falls into ImagePullBackOff once a pod reschedules; this heals it
without recreating the env. --no-roll only refreshes the secret (kubelet picks it
up on its next pull retry). Needs -c with the same config used to create the env
(to re-resolve the ECR registry/region and kube context).`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			reg, err := openRegistry(*regPath)
			if err != nil {
				return &exitError{code: 1, err: err}
			}
			entry, ok, err := reg.Get(args[0])
			if err != nil {
				return &exitError{code: 1, err: err}
			}
			if !ok {
				return &exitError{code: 3, err: fmt.Errorf("env %s not found", args[0])}
			}
			if entry.Provider != "ampstack" {
				return &exitError{code: 2, err: fmt.Errorf("env %s is provider %q; refresh-secret only applies to ampstack k8s envs (other shapes use public images)", args[0], entry.Provider)}
			}
			ns := entry.Data["namespace"]
			if ns == "" {
				return &exitError{code: 2, err: fmt.Errorf("env %s has no recorded namespace", args[0])}
			}
			loadConfig := core.LoadConfig
			if noDefaults {
				loadConfig = core.LoadConfigNoDefaults
			}
			cfg, cerr := loadConfig(configFiles, jsonConfigs, overrides)
			if cerr != nil {
				return &exitError{code: 2, err: cerr}
			}
			acfg := ampstack.FromConfig(cfg)
			acfg.Namespace = ns
			if kctx := entry.Data["context"]; kctx != "" {
				acfg.Context = kctx
			}
			if err := ampstack.New(acfg).RefreshPullSecret(c.Context(), !noRoll); err != nil {
				return &exitError{code: 1, err: fmt.Errorf("refresh pull secret for %s: %w", args[0], err)}
			}
			if jsonOutput {
				return printEnvelope(c.OutOrStdout(), "ampqa.env.refresh-secret", "env refresh-secret",
					map[string]any{"id": args[0], "namespace": ns, "rolled": !noRoll})
			}
			suffix := " + rolled controller"
			if noRoll {
				suffix = ""
			}
			fmt.Fprintf(c.OutOrStdout(), "refreshed pull secret for %s (namespace %s)%s\n", args[0], ns, suffix)
			return nil
		},
	}
	cmd.Flags().StringArrayVarP(&configFiles, "config", "c", nil, "YAML/HOCON config file, deep-merged in order (repeatable)")
	cmd.Flags().StringArrayVar(&jsonConfigs, "jc", nil, "JSON config file, merged after -c (repeatable)")
	cmd.Flags().StringArrayVarP(&overrides, "option", "o", nil, "key=value override with typed prefixes (repeatable)")
	cmd.Flags().BoolVar(&noDefaults, "no-default-config", false, "skip the embedded configs/default.yaml base layer")
	cmd.Flags().BoolVar(&noRoll, "no-roll", false, "only refresh the secret; don't roll the controller")
	return cmd
}

// newEnvReapCmd builds `ampqa env reap`: the recovery tool for cloud
// infrastructure a killed or lost run leaked. It drives the cloud driver's own
// ownership-tag / age-based safety nets — ReapByTag terminates a specific run's
// instances (`--tag`), ReapStale terminates aged autoclean instances whose run
// tag is unknown (`--stale`). It is the CLI that was missing for these
// long-existing but un-wired reap functions. (k8s-namespace recovery for a
// recorded standing/run env goes through `ampqa env rm <id>`, which executes the
// recorded reap.Plan.)
func newEnvReapCmd() *cobra.Command {
	var (
		configFiles []string
		jsonConfigs []string
		overrides   []string
		cloudName   string
		stale       time.Duration
		tag         string
		noDefaults  bool
	)
	cmd := &cobra.Command{
		Use:   "reap",
		Short: "Reap leaked cloud infrastructure from killed or lost runs",
		Long: `Reap cloud infrastructure that a killed or lost run left behind, using the
cloud driver's ownership-tag and age-based safety nets:

  --tag <runTag>   terminate every instance of one run's ownership tag
                   (ampqa-run=<runTag>) — the recovery for a specific killed run.
  --stale <dur>    terminate managed autoclean instances older than <dur> whose
                   run tag is unknown (an operator lost track of a run).

At least one of --tag / --stale is required; both may be given. The cloud is
--cloud (default: env.vm.cloud, else aws).`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			staleSet := c.Flags().Changed("stale")
			if tag == "" && !staleSet {
				return &exitError{code: 2, err: fmt.Errorf("env reap needs --tag <runTag> and/or --stale <dur>")}
			}
			loadConfig := core.LoadConfig
			if noDefaults {
				loadConfig = core.LoadConfigNoDefaults
			}
			cfg, err := loadConfig(configFiles, jsonConfigs, overrides)
			if err != nil {
				return &exitError{code: 2, err: err}
			}
			secrets, err := core.NewSecrets(cfg)
			if err != nil {
				return &exitError{code: 2, err: err}
			}
			cl := cloudName
			if cl == "" {
				cl = cfg.GetString("env.vm.cloud", "aws")
			}
			vm, err := cloudVMForReap(cl, cfg, secrets.Value)
			if err != nil {
				return &exitError{code: 2, err: err}
			}

			var (
				done []string
				errs []string
			)
			if tag != "" {
				if rerr := vm.ReapByTag(c.Context(), tag); rerr != nil {
					errs = append(errs, fmt.Sprintf("reap by tag %s: %v", tag, rerr))
				} else {
					done = append(done, "tag="+tag)
				}
			}
			if staleSet {
				if rerr := vm.ReapStale(c.Context(), stale); rerr != nil {
					errs = append(errs, fmt.Sprintf("reap stale >%s: %v", stale, rerr))
				} else {
					done = append(done, "stale>"+stale.String())
				}
			}
			if len(errs) > 0 {
				return &exitError{code: 1, err: fmt.Errorf("env reap incomplete (%s): %s", cl, strings.Join(errs, "; "))}
			}
			if jsonOutput {
				return printEnvelope(c.OutOrStdout(), "ampqa.env.reap", "env reap", map[string]any{"cloud": cl, "reaped": done})
			}
			fmt.Fprintf(c.OutOrStdout(), "reaped on %s: %s\n", cl, strings.Join(done, ", "))
			return nil
		},
	}
	cmd.Flags().StringArrayVarP(&configFiles, "config", "c", nil, "YAML/HOCON config file, deep-merged in order (repeatable)")
	cmd.Flags().StringArrayVar(&jsonConfigs, "jc", nil, "JSON config file, merged after -c (repeatable)")
	cmd.Flags().StringArrayVarP(&overrides, "option", "o", nil, "key=value override with typed prefixes (repeatable)")
	cmd.Flags().StringVar(&cloudName, "cloud", "", "cloud to reap: aws|gcp (default: env.vm.cloud, else aws)")
	cmd.Flags().DurationVar(&stale, "stale", 0, "reap managed autoclean instances older than this (age-based net)")
	cmd.Flags().StringVar(&tag, "tag", "", "reap every instance of this run's ownership tag (ampqa-run=<tag>)")
	cmd.Flags().BoolVar(&noDefaults, "no-default-config", false, "skip the embedded configs/default.yaml base layer (debugging only)")
	return cmd
}

func openRegistry(path string) (*registry.Registry, error) {
	if path == "" {
		p, err := registry.DefaultPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	return registry.New(path), nil
}

func newEnvLsCmd(regPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List standing environments",
		RunE: func(c *cobra.Command, _ []string) error {
			reg, err := openRegistry(*regPath)
			if err != nil {
				return &exitError{code: 1, err: err}
			}
			entries, err := reg.List()
			if err != nil {
				return &exitError{code: 1, err: err}
			}
			if jsonOutput {
				rows := make([]map[string]any, 0, len(entries))
				for _, e := range entries {
					rows = append(rows, envRow(e))
				}
				return printEnvelope(c.OutOrStdout(), "ampqa.env.list", "env ls", map[string]any{"count": len(entries), "envs": rows})
			}
			out := c.OutOrStdout()
			for _, e := range entries {
				fmt.Fprintf(out, "%-10s name=%-20s shape=%-14s provider=%-10s created=%s ttl=%s\n",
					e.ID, e.Name, e.Shape, e.Provider, e.CreatedAt.Format(time.RFC3339), e.TTL)
			}
			fmt.Fprintf(out, "%d environment(s)\n", len(entries))
			return nil
		},
	}
}

func newEnvRmCmd(regPath *string) *cobra.Command {
	var (
		configFiles []string
		jsonConfigs []string
		overrides   []string
		envProvider string
		skipTD      bool
		force       bool
		noDefaults  bool
	)
	cmd := &cobra.Command{
		Use:   "rm <id>",
		Short: "Tear a standing environment down and remove it from the registry",
		Long: `Reconstruct the standing environment's REAL teardown from its registry
record and run it (cloud/universe/YBA/VM reap), then remove the record. Use
--skip-teardown to only drop the record (leaving any infrastructure running),
or --force to remove the record even when teardown errors.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			reg, err := openRegistry(*regPath)
			if err != nil {
				return &exitError{code: 1, err: err}
			}
			entry, ok, err := reg.Get(args[0])
			if err != nil {
				return &exitError{code: 1, err: err}
			}
			if !ok {
				return &exitError{code: 3, err: fmt.Errorf("env %s not found", args[0])}
			}

			var tdErr error
			if !skipTD {
				loadConfig := core.LoadConfig
				if noDefaults {
					loadConfig = core.LoadConfigNoDefaults
				}
				cfg, cerr := loadConfig(configFiles, jsonConfigs, overrides)
				if cerr != nil {
					return &exitError{code: 2, err: cerr}
				}
				secrets, serr := core.NewSecrets(cfg)
				if serr != nil {
					return &exitError{code: 2, err: serr}
				}
				masker := core.NewMaskerFromSecrets(secrets)
				detected := core.DetectedVersions{
					Product: cfg.GetString("versions.product", ""),
					YB:      cfg.GetString("versions.yb", ""),
				}
				tdErr = reapStandingEnv(c.Context(), entry, cfg, secrets, masker, envProvider, detected, force)
				if tdErr != nil && !force {
					return &exitError{code: 1, err: fmt.Errorf("teardown of %s failed (record kept; retry, or --force to remove anyway, or --skip-teardown): %w", args[0], tdErr)}
				}
			}

			removed, err := reg.Remove(args[0])
			if err != nil {
				return &exitError{code: 1, err: err}
			}
			if !removed {
				return &exitError{code: 3, err: fmt.Errorf("env %s not found", args[0])}
			}
			if jsonOutput {
				data := map[string]any{"id": args[0], "removed": true, "teardown_skipped": skipTD}
				if tdErr != nil {
					data["teardown_error"] = tdErr.Error()
				}
				return printEnvelope(c.OutOrStdout(), "ampqa.env.rm", "env rm", data)
			}
			switch {
			case skipTD:
				fmt.Fprintf(c.OutOrStdout(), "removed %s (teardown skipped)\n", args[0])
			case tdErr != nil:
				fmt.Fprintf(c.OutOrStdout(), "removed %s (teardown errored, forced): %v\n", args[0], tdErr)
			default:
				fmt.Fprintf(c.OutOrStdout(), "removed %s (torn down)\n", args[0])
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVarP(&configFiles, "config", "c", nil, "YAML/HOCON config file, deep-merged in order (repeatable)")
	cmd.Flags().StringArrayVar(&jsonConfigs, "jc", nil, "JSON config file, merged after -c (repeatable)")
	cmd.Flags().StringArrayVarP(&overrides, "option", "o", nil, "key=value override with typed prefixes (repeatable)")
	cmd.Flags().StringVar(&envProvider, "env-provider", "", "override the provider used to reconstruct teardown (default: the entry's own)")
	cmd.Flags().BoolVar(&skipTD, "skip-teardown", false, "only drop the registry record; leave infrastructure running")
	cmd.Flags().BoolVar(&force, "force", false, "remove the record even if teardown errors")
	cmd.Flags().BoolVar(&noDefaults, "no-default-config", false, "skip the embedded configs/default.yaml base layer (debugging only)")
	return cmd
}

func envRow(e registry.Entry) map[string]any {
	return map[string]any{
		"id":         e.ID,
		"name":       e.Name,
		"shape":      e.Shape,
		"provider":   e.Provider,
		"created_at": e.CreatedAt.Format(time.RFC3339),
		"ttl":        e.TTL.String(),
		"data":       e.Data,
	}
}
