package perf

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// captureEndpointDiag attaches a dead endpoint's pod status and logs, including
// the instance before a restart. The env's collector only gathers the endpoint
// IT provisioned, so a ladder's own tenant would otherwise leave nothing behind:
// three depth runs failed with "connection refused" and none could say whether
// the pod was evicted, OOM-killed or crashed. Best-effort — a failed diagnostic
// must not replace the failure it explains.
func captureEndpointDiag(t *core.T, kube *k8s.Kubectl, ten tenant, label string, logf func(string, ...any)) string {
	if kube == nil {
		return ""
	}
	pod, err := endpointPodFor(t, kube, ten)
	if err != nil {
		logf("perf: endpoint diagnostics: %v", err)
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "pod: %s\n", pod)

	if raw, err := kube.Get(t.Context, "pod", pod, "{@}"); err == nil {
		fmt.Fprintf(&b, "%s\n", podVerdict(raw))
		t.AttachBytes(fmt.Sprintf("endpoint-pod-%s.json", label), "application/json", []byte(raw))
	} else {
		fmt.Fprintf(&b, "pod status unavailable: %v\n", err)
	}

	// The current instance's log, and the previous one when it restarted —
	// a crash leaves its evidence only in the previous container's log.
	if logs, err := kube.Logs(t.Context, pod, "endpoint", endpointDiagLines); err == nil {
		t.AttachBytes(fmt.Sprintf("endpoint-%s.log", label), "text/plain", []byte(logs))
	} else {
		fmt.Fprintf(&b, "endpoint log unavailable: %v\n", err)
	}
	if prev, err := kube.LogsPrevious(t.Context, pod, "endpoint", endpointDiagLines); err == nil && strings.TrimSpace(prev) != "" {
		t.AttachBytes(fmt.Sprintf("endpoint-%s-previous.log", label), "text/plain", []byte(prev))
		b.WriteString("a previous instance's log was captured: the container restarted\n")
	}
	return b.String()
}

const endpointDiagLines = 4000

// podVerdict summarises the one thing the failure needs: did this pod die, and
// how. Kubernetes says it in three different places depending on the cause.
func podVerdict(raw string) string {
	var p struct {
		Status struct {
			Phase             string `json:"phase"`
			Reason            string `json:"reason"`
			Message           string `json:"message"`
			ContainerStatuses []struct {
				Name         string `json:"name"`
				RestartCount int    `json:"restartCount"`
				LastState    struct {
					Terminated struct {
						Reason     string `json:"reason"`
						ExitCode   int    `json:"exitCode"`
						FinishedAt string `json:"finishedAt"`
					} `json:"terminated"`
				} `json:"lastState"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return "pod status unparseable: " + err.Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "phase=%s", p.Status.Phase)
	if p.Status.Reason != "" {
		// "Evicted" lands here, with the reason (ephemeral storage, memory) in
		// the message.
		fmt.Fprintf(&b, " reason=%s message=%q", p.Status.Reason, p.Status.Message)
	}
	for _, c := range p.Status.ContainerStatuses {
		fmt.Fprintf(&b, "\ncontainer %s restarts=%d", c.Name, c.RestartCount)
		if t := c.LastState.Terminated; t.Reason != "" {
			fmt.Fprintf(&b, " lastTerminated=%s exit=%d at=%s", t.Reason, t.ExitCode, t.FinishedAt)
		}
	}
	return b.String()
}
