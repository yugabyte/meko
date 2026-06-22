// Package helm shells out to the helm CLI for one-shot release operations
// (amp-controller.md §11: pod-based deployments get their own Helm chart).
// `helm upgrade --install` converges to the desired state, which the task
// framework's at-least-once step execution needs (a reclaimed deploy step
// re-runs the same install). But that only holds when the release is in a
// terminal state: a helm process killed mid-upgrade (worker shutdown, lease
// loss, pod restart) leaves the release pending, and helm then refuses the next
// upgrade. UpgradeInstall therefore clears a stuck pending release first — it,
// not bare `upgrade --install`, is what makes the step crash-safe.
//
// Long-lived managed processes use internal/process; helm is short-lived, so
// this is a plain exec-and-capture rather than a supervisor.
//
// Requires helm >= 3.13 (uninstall --ignore-not-found, added in 3.13).
package helm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	osexec "os/exec"
	"strings"
	"time"
)

// Client runs the helm CLI. A bare binary name is resolved by exec's normal
// PATH lookup; a path is used verbatim.
type Client struct {
	bin        string
	kubeconfig string // KUBECONFIG to pass to helm; empty = helm's default discovery
	logger     *slog.Logger
}

// New returns a Client. An empty bin defaults to "helm".
func New(bin, kubeconfig string, logger *slog.Logger) *Client {
	if bin == "" {
		bin = "helm"
	}
	return &Client{bin: bin, kubeconfig: kubeconfig, logger: logger}
}

// Release describes one `helm upgrade --install` invocation.
type Release struct {
	Name            string        // release name (unique per endpoint)
	Chart           string        // chart path or reference
	Namespace       string        // target namespace
	Values          any           // marshaled to a temp values file passed with --values
	CreateNamespace bool          // pass --create-namespace
	Wait            bool          // pass --wait (block until resources are ready)
	Timeout         time.Duration // pass --timeout; 0 = helm's default
}

// UpgradeInstall runs `helm upgrade --install`, creating the release if absent
// and updating it otherwise. It first clears a release left in a pending state
// by an interrupted prior attempt, so a re-run converges rather than wedging.
// It is idempotent.
func (c *Client) UpgradeInstall(ctx context.Context, r Release) error {
	if err := c.recoverPending(ctx, r.Name, r.Namespace); err != nil {
		return err
	}
	valuesPath, cleanup, err := writeValues(r.Values)
	if err != nil {
		return fmt.Errorf("render helm values: %w", err)
	}
	defer cleanup()
	_, err = c.run(ctx, upgradeArgs(r, valuesPath))
	return err
}

// recoverPending uninstalls a release stuck in a pending state (the residue of
// an upgrade/install killed before it committed) so the following
// UpgradeInstall can proceed. A release that does not exist — or whose status
// is otherwise unavailable — is left for the install to handle.
func (c *Client) recoverPending(ctx context.Context, release, namespace string) error {
	out, err := c.run(ctx, []string{"status", release, "--namespace", namespace, "--output", "json"})
	if err != nil {
		return nil // release absent or status unavailable; a fresh install proceeds
	}
	var status struct {
		Info struct {
			Status string `json:"status"`
		} `json:"info"`
	}
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		return nil
	}
	switch status.Info.Status {
	case "pending-install", "pending-upgrade", "pending-rollback", "uninstalling":
		if c.logger != nil {
			c.logger.Warn("helm release stuck pending; uninstalling before reinstall",
				"release", release, "status", status.Info.Status)
		}
		return c.Uninstall(ctx, release, namespace, true, 0)
	}
	return nil
}

// Uninstall removes a release. It tolerates an already-absent release so a
// reclaimed retire step re-runs cleanly.
func (c *Client) Uninstall(ctx context.Context, release, namespace string, wait bool, timeout time.Duration) error {
	args := []string{"uninstall", release, "--namespace", namespace, "--ignore-not-found"}
	if wait {
		args = append(args, "--wait")
	}
	if timeout > 0 {
		args = append(args, "--timeout", timeout.String())
	}
	_, err := c.run(ctx, args)
	return err
}

// upgradeArgs builds the argv (excluding the binary) for an upgrade --install.
// Split out from UpgradeInstall so the argument construction is unit-testable
// without invoking helm.
func upgradeArgs(r Release, valuesPath string) []string {
	args := []string{"upgrade", "--install", r.Name, r.Chart, "--namespace", r.Namespace}
	if r.CreateNamespace {
		args = append(args, "--create-namespace")
	}
	if valuesPath != "" {
		args = append(args, "--values", valuesPath)
	}
	if r.Wait {
		args = append(args, "--wait")
	}
	if r.Timeout > 0 {
		args = append(args, "--timeout", r.Timeout.String())
	}
	return args
}

func (c *Client) run(ctx context.Context, args []string) (string, error) {
	cmd := osexec.CommandContext(ctx, c.bin, args...)
	if c.kubeconfig != "" {
		cmd.Env = append(os.Environ(), "KUBECONFIG="+c.kubeconfig)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if c.logger != nil {
		c.logger.Debug("running helm", "args", strings.Join(args, " "))
	}
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("helm %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// writeValues marshals v to a temp file helm reads with --values. JSON is a
// subset of YAML, so a JSON document is a valid helm values file; this avoids
// a YAML dependency and the --set escaping pitfalls for values that contain
// dots, slashes, or colons (image refs, host:port lists).
func writeValues(v any) (path string, cleanup func(), err error) {
	cleanup = func() {}
	if v == nil {
		return "", cleanup, nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return "", cleanup, err
	}
	f, err := os.CreateTemp("", "controller-helm-values-*.yaml")
	if err != nil {
		return "", cleanup, err
	}
	name := f.Name()
	cleanup = func() { _ = os.Remove(name) }
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return name, cleanup, nil
}
