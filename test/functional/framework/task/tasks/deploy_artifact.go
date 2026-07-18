package tasks

import (
	"context"
	"fmt"

	"github.com/yugabyte/amp/test/functional/framework/artifact"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/task"
)

var deployArtifactTask = task.Task{
	Name: "deploy-artifact",
	Doc:  "Resolve one declared artifact and deploy it to one host over ssh (exercises artifact/ end-to-end).",
	Params: map[string]string{
		"task.artifact":   "artifact name (must be declared under artifacts{} in config)",
		"task.host":       "ssh host",
		"task.ssh_user":   "ssh user (default: current OS user)",
		"task.key":        "ssh private key path",
		"task.label":      "target scope label matched against the artifact's target (default: matches any)",
		"task.remote_dir": "directory on the target the artifact lands in",
	},
	Run: func(ctx context.Context, deps task.Deps) error {
		name := deps.Config.GetString("task.artifact", "")
		if name == "" {
			return fmt.Errorf("deploy-artifact: task.artifact is required")
		}
		host := deps.Config.GetString("task.host", "")
		if host == "" {
			return fmt.Errorf("deploy-artifact: task.host is required")
		}
		remoteDir := deps.Config.GetString("task.remote_dir", "")
		if remoteDir == "" {
			return fmt.Errorf("deploy-artifact: task.remote_dir is required")
		}

		specs, err := artifact.Resolve(deps.Config)
		if err != nil {
			return fmt.Errorf("resolve artifacts: %w", err)
		}
		spec, ok := specs[name]
		if !ok {
			return fmt.Errorf("deploy-artifact: %q is not declared under artifacts{}", name)
		}
		var secret string
		if spec.Secret != "" {
			secrets, err := core.NewSecrets(deps.Config)
			if err != nil {
				return fmt.Errorf("resolve secrets: %w", err)
			}
			secret, err = secrets.Value(spec.Secret)
			if err != nil {
				return fmt.Errorf("artifact %q secret: %w", name, err)
			}
		}

		tr, err := defaultDial(ctx, host, deps.Config.GetString("task.ssh_user", ""), deps.Config.GetString("task.key", ""))
		if err != nil {
			return fmt.Errorf("dial %s: %w", host, err)
		}
		defer tr.Close()

		d := &artifact.Deployer{CacheDir: deps.Config.GetString("artifact.cache_dir", "")}
		return d.Deploy(ctx, []exec.RemoteExecutor{tr}, spec, artifact.DeployOptions{
			RemoteDir: remoteDir,
			Label:     deps.Config.GetString("task.label", ""),
			Secret:    secret,
		})
	},
}
