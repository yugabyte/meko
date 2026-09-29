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
	"errors"
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
	Name      string        // release name (unique per endpoint)
	Chart     string        // chart path or reference
	Namespace string        // target namespace (must already exist)
	Values    any           // marshaled to a temp values file passed with --values
	Wait      bool          // pass --wait (block until resources are ready)
	Timeout   time.Duration // pass --timeout; 0 = helm's default
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
	if _, err := c.run(ctx, "upgrade --install "+r.Name, upgradeArgs(r, valuesPath)); err != nil {
		return waitTimeout(r, err)
	}
	return nil
}

// ErrWaitTimeout marks the one failure where the workload, not helm, holds the
// reason: --wait ran out with the release still not ready. Callers enrich on
// exactly this (errors.Is) — every other way an upgrade fails (a chart render
// refusal, an unwritable values file, a cancelled context) has its cause in
// helm's own message and no pod to read.
var ErrWaitTimeout = errors.New("helm --wait expired")

// waitTimeout renames helm's timeout for what it is and tags it. When --wait
// runs out helm exits with "Error: context deadline exceeded", which names Go's
// plumbing rather than the deploy and reads — to an operator, and to an AI
// handed the task row — as a request-context bug somewhere else entirely
// (#635). The stderr is matched rather than the exit status because only that
// distinguishes the timeout from every other way helm exits 1.
func waitTimeout(r Release, err error) error {
	if !r.Wait || r.Timeout == 0 || !strings.Contains(err.Error(), "context deadline exceeded") {
		return err
	}
	return fmt.Errorf("release %s did not become ready within %s (%w): %w", r.Name, r.Timeout, ErrWaitTimeout, err)
}

// recoverPending uninstalls a release stuck in a pending state (the residue of
// an upgrade/install killed before it committed) so the following
// UpgradeInstall can proceed. A release that does not exist — or whose status
// is otherwise unavailable — is left for the install to handle.
func (c *Client) recoverPending(ctx context.Context, release, namespace string) error {
	out, err := c.run(ctx, "status "+release, []string{"status", release, "--namespace", namespace, "--output", "json"})
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
// reclaimed retire step re-runs cleanly, and a release another uninstall
// removed while this one was running (see lostPurgeRace).
func (c *Client) Uninstall(ctx context.Context, release, namespace string, wait bool, timeout time.Duration) error {
	args := []string{"uninstall", release, "--namespace", namespace, "--ignore-not-found"}
	if wait {
		args = append(args, "--wait")
	}
	if timeout > 0 {
		args = append(args, "--timeout", timeout.String())
	}
	_, err := c.run(ctx, "uninstall "+release, args)
	if err != nil && lostPurgeRace(err) {
		if c.logger != nil {
			c.logger.Info("helm release removed by a concurrent uninstall", "release", release)
		}
		return nil
	}
	return err
}

// lostPurgeRace reports helm's answer when two uninstalls of one release
// overlap and this one lost the final purge: the release record it loaded was
// already purged by the other. --ignore-not-found does not cover it, because
// the release existed when this uninstall started. The release is gone, which
// is what the caller asked for. Seen when a cluster delete follows its
// endpoint's delete back to back.
//
// helm (pkg/action/uninstall.go, 3.18) reports it two ways, and both qualify:
//   - it deleted the resources, then the purge failed: "uninstallation
//     completed with 1 error(s): uninstall: Failed to purge the release:
//     release: not found". Only a count of 1 qualifies; with any other error
//     alongside, the failure still surfaces.
//   - the release was already marked uninstalled, so it went straight to the
//     purge: "uninstall: Failed to purge the release: release: not found",
//     with no count.
func lostPurgeRace(err error) bool {
	msg := err.Error()
	if !strings.Contains(msg, "uninstall: Failed to purge the release: release: not found") {
		return false
	}
	if !strings.Contains(msg, "uninstallation completed with") {
		return true
	}
	return strings.Contains(msg, "uninstallation completed with 1 error(s)")
}

// upgradeArgs builds the argv (excluding the binary) for an upgrade --install.
// Split out from UpgradeInstall so the argument construction is unit-testable
// without invoking helm.
func upgradeArgs(r Release, valuesPath string) []string {
	args := []string{"upgrade", "--install", r.Name, r.Chart, "--namespace", r.Namespace}
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

// run executes helm and, on failure, reports `what` plus helm's stderr. `what`
// is the operation in the operator's terms ("upgrade --install yb-ep-<id>"),
// NOT the argv: the full command echoes the chart path and a temp values file,
// controller internals nobody reading a failed task can act on, which crowd out
// the part of the error that matters (#635). The argv stays on the debug log
// below, where a controller developer can find it.
func (c *Client) run(ctx context.Context, what string, args []string) (string, error) {
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
			what, err, strings.TrimSpace(stderr.String()))
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
