package mekostack

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	execpkg "github.com/yugabyte/meko/meko/test/functional/framework/exec"
	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
)

// Release states a half-finished helm operation leaves behind. The next
// upgrade refuses to run against any of them.
const (
	statusPendingInstall  = "pending-install"
	statusPendingUpgrade  = "pending-upgrade"
	statusPendingRollback = "pending-rollback"
)

func (a *App) helm(ctx context.Context, args ...string) (execpkg.Result, error) {
	argv := []string{"helm"}
	if a.cfg.Context != "" {
		argv = append(argv, "--kube-context", a.cfg.Context)
	}
	argv = append(argv, args...)
	a.log("exec: %s", k8s.CmdString(argv))
	res, err := a.helmRun(ctx, nil, argv)
	if err != nil {
		return res, fmt.Errorf("helm %s: %w", args[0], err)
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("helm %s exited %d: %s", args[0], res.ExitCode, tail(res.Stderr+res.Stdout, 2000))
	}
	return res, nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// chartArgs is the part of `helm template` and `helm upgrade` that names the
// chart and what is laid over it: the presets in order, then the generated file.
func (a *App) chartArgs(valuesPath string) []string {
	args := []string{a.cfg.Release, a.cfg.ChartPath, "-n", a.cfg.Namespace}
	for _, f := range a.cfg.ValuesFiles {
		args = append(args, "-f", filepath.Join(a.cfg.ChartPath, f))
	}
	return append(args, "-f", valuesPath)
}

func (a *App) template(ctx context.Context, valuesPath string) (string, error) {
	res, err := a.helm(ctx, append([]string{"template"}, a.chartArgs(valuesPath)...)...)
	if err != nil {
		return "", err
	}
	return res.Stdout, nil
}

// upgrade does not pass --wait: the api-server and langfuse run their
// migrations in init containers, which on a fresh bundled database outlast the
// Deployments' 600s progress deadline, and helm reads that condition as a failed
// install although the pods go on to start. waitWorkloads does the waiting.
func (a *App) upgrade(ctx context.Context, valuesPath string) error {
	args := append([]string{"upgrade", "--install"}, a.chartArgs(valuesPath)...)
	args = append(args, "--timeout", durationArg(a.cfg.ProvisionTimeout))
	_, err := a.helm(ctx, args...)
	return err
}

type helmRelease struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// releaseStatus reads the release's state from `helm list`, which unlike
// `helm status` also lists a release stuck in a pending state. The status
// filters are explicit because helm 4 dropped -a and helm 3 hides pending
// releases without it.
func (a *App) releaseStatus(ctx context.Context) (status string, found bool, err error) {
	res, err := a.helm(ctx, "list", "--deployed", "--failed", "--pending", "--superseded", "--uninstalling", "-n", a.cfg.Namespace, "-o", "json")
	if err != nil {
		return "", false, err
	}
	var rels []helmRelease
	if out := strings.TrimSpace(res.Stdout); out != "" {
		if err := json.Unmarshal([]byte(out), &rels); err != nil {
			return "", false, fmt.Errorf("decode helm list: %w", err)
		}
	}
	for _, r := range rels {
		if r.Name == a.cfg.Release {
			return r.Status, true, nil
		}
	}
	return "", false, nil
}

// repairRelease clears a pending state a previous attempt of this framework
// left, so the upgrade that follows is not refused for it. The namespace is
// ours (Deploy checked), so the release in it is too.
func (a *App) repairRelease(ctx context.Context) error {
	status, found, err := a.releaseStatus(ctx)
	if err != nil || !found {
		return err
	}
	switch status {
	case statusPendingInstall:
		a.log("mekostack: release %s is %s from an earlier attempt, uninstalling it", a.cfg.Release, status)
		_, err = a.helm(ctx, "uninstall", a.cfg.Release, "-n", a.cfg.Namespace, "--timeout", "5m")
	case statusPendingUpgrade, statusPendingRollback:
		a.log("mekostack: release %s is %s from an earlier attempt, rolling it back", a.cfg.Release, status)
		_, err = a.helm(ctx, "rollback", a.cfg.Release, "-n", a.cfg.Namespace, "--timeout", "10m")
	}
	return err
}

func durationArg(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	return fmt.Sprintf("%ds", int(d.Round(time.Second)/time.Second))
}
