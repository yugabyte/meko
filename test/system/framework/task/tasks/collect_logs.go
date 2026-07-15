package tasks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yugabyte/yb-amp/test/system/framework/apps/dockerhost"
	"github.com/yugabyte/yb-amp/test/system/framework/task"
)

// CollectLogs ssh's to host and writes `docker logs` for each container into
// outDir/<container>.log. dial defaults to a real ssh.Dial when nil.
func CollectLogs(ctx context.Context, dial dialFunc, host, sshUser, sshKey string, containers []string, outDir string) error {
	if dial == nil {
		dial = defaultDial
	}
	tr, err := dial(ctx, host, sshUser, sshKey)
	if err != nil {
		return fmt.Errorf("dial %s: %w", host, err)
	}
	defer tr.Close()
	dh := dockerhost.New(tr)

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", outDir, err)
	}
	for _, name := range containers {
		logs, err := dh.Logs(ctx, name, 0)
		if err != nil {
			return fmt.Errorf("logs %s: %w", name, err)
		}
		dest := filepath.Join(outDir, name+".log")
		if err := os.WriteFile(dest, []byte(logs), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dest, err)
		}
	}
	return nil
}

var collectLogsTask = task.Task{
	Name: "collect-logs",
	Doc:  "Fetch `docker logs` for named containers on a host into local files.",
	Params: map[string]string{
		"task.host":       "ssh host",
		"task.ssh_user":   "ssh user (default: current OS user)",
		"task.key":        "ssh private key path",
		"task.containers": "list: comma-separated container names",
		"task.out":        "output directory",
	},
	Run: func(ctx context.Context, deps task.Deps) error {
		host := deps.Config.GetString("task.host", "")
		if host == "" {
			return fmt.Errorf("collect-logs: task.host is required")
		}
		containers := deps.Config.GetStringSlice("task.containers", nil)
		if len(containers) == 0 {
			return fmt.Errorf("collect-logs: task.containers is required")
		}
		out := deps.Config.GetString("task.out", "")
		if out == "" {
			return fmt.Errorf("collect-logs: task.out is required")
		}
		return CollectLogs(ctx, nil, host, deps.Config.GetString("task.ssh_user", ""), deps.Config.GetString("task.key", ""), containers, out)
	},
}
