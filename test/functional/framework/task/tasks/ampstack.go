package tasks

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/env/registry"
	"github.com/yugabyte/amp/test/functional/framework/task"
)

func init() {
	task.Register(deployAmpTask)
	task.Register(statusAmpTask)
	task.Register(teardownAmpTask)
}

// ampstackParams documents the -o keys the amp-stack tasks read (a subset of
// env.ampstack.*, plus task.registry / task.env_id).
var ampstackParams = map[string]string{
	"env.ampstack.namespace":           "target namespace (required)",
	"env.ampstack.context":             "kube-context (default: current)",
	"env.ampstack.controller_chart":    "path to the yb-amp-controller Helm chart (required for deploy-amp)",
	"env.ampstack.controller_image":    "controller image ref (default: config / versions.controller)",
	"env.ampstack.endpoint_image":      "endpoint image ref (default: config / versions.endpoint)",
	"env.ampstack.rbac_cluster_scoped": "bool: create ClusterRole/ClusterRoleBinding (default false)",
	"env.ampstack.helm_shim":           "bool: install the --create-namespace-stripping helm shim (default true)",
	"env.ampstack.storage_class":       "endpoint /data PVC storage class (default: emptyDir)",
	"env.ampstack.pull_secret_b64":     "pre-encoded .dockerconfigjson; empty → mint via aws ecr get-login-password",
	"task.registry":                    "registry file path (default ~/.ampqa/envs.json)",
	"task.env_id":                      "registry entry id (teardown-amp; default: match by namespace)",
}

// registryFor opens the registry at task.registry, or the default path.
func registryFor(deps task.Deps) (*registry.Registry, error) {
	path := deps.Config.GetString("task.registry", "")
	if path == "" {
		p, err := registry.DefaultPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	return registry.New(path), nil
}

func logfOf(deps task.Deps) func(string, ...any) {
	if deps.Logf != nil {
		return deps.Logf
	}
	return func(string, ...any) {}
}

var deployAmpTask = task.Task{
	Name:   "deploy-amp",
	Doc:    "Stand up an amp control plane in a namespace (apps/ampstack), record it in the env registry, and print how to reach it.",
	Params: ampstackParams,
	Run: func(ctx context.Context, deps task.Deps) error {
		cfg := ampstack.FromConfig(deps.Config)
		if cfg.Namespace == "" {
			return fmt.Errorf("deploy-amp: env.ampstack.namespace is required")
		}
		app := ampstack.New(cfg)
		info, err := app.Deploy(ctx)
		if err != nil {
			return err
		}
		// The health-check port-forward can't outlive this process, so release it
		// and hand the operator the command to re-establish it.
		_ = app.Close(ctx)

		reg, err := registryFor(deps)
		if err != nil {
			return err
		}
		entry, err := reg.Add(registry.Entry{
			Name:     info.Namespace,
			Shape:    "dedicated_stack",
			Provider: "ampstack",
			Data: map[string]string{
				"namespace":          info.Namespace,
				"context":            cfg.Context,
				"controller_service": info.ControllerService,
				"controller_port":    strconv.Itoa(info.ControllerPort),
				"port_forward":       info.PortForwardCmd,
				"images":             joinImages(info.Images),
			},
		})
		if err != nil {
			return fmt.Errorf("record stack in registry: %w", err)
		}

		logf := logfOf(deps)
		logf("deploy-amp: stack up in namespace %s (registry id %s)", info.Namespace, entry.ID)
		logf("  images: %s", joinImages(info.Images))
		logf("  reach the controller: %s   then http://localhost:%d", info.PortForwardCmd, info.ControllerPort)
		return nil
	},
}

var statusAmpTask = task.Task{
	Name:   "status-amp",
	Doc:    "Report readiness of an amp control plane (pods, yugabyted, controller /healthz).",
	Params: ampstackParams,
	Run: func(ctx context.Context, deps task.Deps) error {
		cfg := ampstack.FromConfig(deps.Config)
		if cfg.Namespace == "" {
			return fmt.Errorf("status-amp: env.ampstack.namespace is required")
		}
		st, err := ampstack.New(cfg).Status(ctx)
		if err != nil {
			return err
		}
		logf := logfOf(deps)
		logf("status-amp: namespace=%s yugabyted_ready=%t controller_ready=%t healthy=%t",
			st.Namespace, st.YugabytedReady, st.ControllerReady, st.Healthy)
		for _, pod := range sortedKeys(st.Pods) {
			logf("  pod %s: %s", pod, st.Pods[pod])
		}
		return nil
	},
}

var teardownAmpTask = task.Task{
	Name:   "teardown-amp",
	Doc:    "Tear down an amp control plane (label-guarded namespace delete) and drop its env-registry entry.",
	Params: ampstackParams,
	Run: func(ctx context.Context, deps task.Deps) error {
		cfg := ampstack.FromConfig(deps.Config)
		if cfg.Namespace == "" {
			return fmt.Errorf("teardown-amp: env.ampstack.namespace is required")
		}
		if err := ampstack.New(cfg).Teardown(ctx); err != nil {
			return err
		}

		reg, err := registryFor(deps)
		if err != nil {
			return err
		}
		id, err := resolveRegistryEntry(reg, deps.Config.GetString("task.env_id", ""), cfg.Namespace)
		if err != nil {
			return err
		}
		logf := logfOf(deps)
		if id == "" {
			logf("teardown-amp: namespace %s deleted (no registry entry to remove)", cfg.Namespace)
			return nil
		}
		if _, err := reg.Remove(id); err != nil {
			return fmt.Errorf("remove registry entry %s: %w", id, err)
		}
		logf("teardown-amp: namespace %s deleted, registry entry %s removed", cfg.Namespace, id)
		return nil
	},
}

// resolveRegistryEntry returns the id to remove: envID when given, else the id
// of the ampstack entry whose namespace matches, else "" (none).
func resolveRegistryEntry(reg *registry.Registry, envID, namespace string) (string, error) {
	if envID != "" {
		return envID, nil
	}
	entries, err := reg.List()
	if err != nil {
		return "", fmt.Errorf("list registry: %w", err)
	}
	for _, e := range entries {
		if e.Provider == "ampstack" && e.Data["namespace"] == namespace {
			return e.ID, nil
		}
	}
	return "", nil
}

func joinImages(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for _, k := range sortedKeys(m) {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ", ")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
