package perf

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

// From inside the pod, because the coordinator's port-forward adds ~29ms per round trip.
const endpointPsql = "psql -U cloud_admin -h 127.0.0.1 -p 55433 -d postgres -Atc"

type ladderSteps struct {
	Start     int
	Factor    float64
	MaxStages int
}

func ladderStepsFromConfig(c *core.Config, prefix string, start int, factor float64, maxStages int) ladderSteps {
	return ladderSteps{
		Start:     c.GetInt(prefix+".start", start),
		Factor:    c.GetFloat(prefix+".factor", factor),
		MaxStages: c.GetInt(prefix+".max_stages", maxStages),
	}
}

func (l ladderSteps) rung(i int) int {
	n := int(float64(l.Start) * math.Pow(l.Factor, float64(i)))
	return max(n, l.Start)
}

// A ladder out of stages would otherwise publish its own config as the product's limit. A non-empty stopReason means the wall clock ended it, which max_stages would not change.
func noCeilingReached(stages int, reached, stopReason string) error {
	if stopReason != "" {
		return fmt.Errorf("the ladder ran out of its wall-clock budget before anything broke (%s): %s is not a limit", stopReason, reached)
	}
	return fmt.Errorf("no ceiling reached in %d stages (%s): raise max_stages or tighten the stop condition, because the last rung is not a limit", stages, reached)
}

var sqlstatePattern = regexp.MustCompile(`SQLSTATE ([0-9A-Z]{5})`)

func sqlstateOf(err error) string {
	if err == nil {
		return ""
	}
	if m := sqlstatePattern.FindStringSubmatch(err.Error()); m != nil {
		return m[1]
	}
	return ""
}

type endpointHealth struct {
	restarts map[string]int
	pids     map[string]string
}

func snapshotEndpointHealth(ctx context.Context, kube *k8s.Kubectl, pod string) (endpointHealth, error) {
	h := endpointHealth{restarts: map[string]int{}, pids: map[string]string{}}
	raw, err := kube.Get(ctx, "pod", pod, "{@}")
	if err != nil {
		return h, err
	}
	var p struct {
		Status struct {
			ContainerStatuses []struct {
				Name         string `json:"name"`
				RestartCount int    `json:"restartCount"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return h, fmt.Errorf("parse pod %s: %w", pod, err)
	}
	for _, c := range p.Status.ContainerStatuses {
		h.restarts[c.Name] = c.RestartCount
	}
	ex := kube.ForPod(pod, "endpoint")
	for _, daemon := range []string{"pageserver", "safekeeper"} {
		pids, err := daemonPIDLine(ctx, ex, daemon)
		if err != nil {
			return h, err
		}
		h.pids[daemon] = pids
	}
	return h, nil
}

// pgrep exits 1 when nothing matched, which is an answer. Any other non-zero is a scan that did not happen, not "the daemon is gone".
func daemonPIDLine(ctx context.Context, ex *k8s.Kubectl, daemon string) (string, error) {
	res, err := ex.Run(ctx, "pgrep -x "+daemon)
	if err != nil {
		return "", fmt.Errorf("list %s pids: %w", daemon, err)
	}
	if res.ExitCode > 1 {
		return "", fmt.Errorf("listing %s pids in the pod exited %d: %s", daemon, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	fields := strings.Fields(res.Stdout)
	sort.Strings(fields)
	return strings.Join(fields, " "), nil
}

func (h endpointHealth) changesFrom(base endpointHealth) []string {
	var out []string
	for name, n := range h.restarts {
		if was, ok := base.restarts[name]; ok && n > was {
			out = append(out, fmt.Sprintf("container %s restarted %d time(s) during the climb", name, n-was))
		}
	}
	for daemon, pids := range h.pids {
		was, ok := base.pids[daemon]
		if !ok {
			out = append(out, daemon+" was not in the baseline scan, so its pids could not be compared")
			continue
		}
		if pids == was {
			continue
		}
		if pids == "" {
			out = append(out, daemon+" is gone")
			continue
		}
		out = append(out, fmt.Sprintf("%s was respawned (pids %q, was %q)", daemon, pids, was))
	}
	sort.Strings(out)
	return out
}

type cleanCheck struct {
	name string
	err  error
}

func assertCleanCeiling(ctx context.Context, s *core.StepHandle, kube *k8s.Kubectl, pod string, base endpointHealth, extra ...cleanCheck) error {
	if kube == nil || pod == "" {
		s.Set("clean_failure", "inconclusive: no kube context, so no restart, daemon or serving check ran")
		return fmt.Errorf("the ceiling could not be shown to be clean: this run has no kube context for the endpoint pod")
	}
	var dirty, unknown []string
	now, err := snapshotEndpointHealth(ctx, kube, pod)
	if err != nil {
		unknown = append(unknown, "the restart and daemon scan did not run: "+err.Error())
	} else {
		dirty = append(dirty, now.changesFrom(base)...)
	}
	res, err := kube.ForPod(pod, "endpoint").Run(ctx, "timeout 60 "+endpointPsql+" 'select 1'")
	switch {
	case err != nil:
		unknown = append(unknown, "the serving probe never reached the pod: "+err.Error())
	case res.ExitCode != 0:
		dirty = append(dirty, fmt.Sprintf("the endpoint no longer answers SQL at the ceiling (exit %d): %s",
			res.ExitCode, strings.TrimSpace(res.Stderr)))
	}
	for _, c := range extra {
		if c.err != nil {
			dirty = append(dirty, c.name+": "+c.err.Error())
		}
	}
	if len(dirty) > 0 {
		s.Set("clean_failure", "no: "+strings.Join(dirty, "; "))
		return fmt.Errorf("the ceiling was reached, but the way the product failed there is not acceptable: %s", strings.Join(dirty, "; "))
	}
	// A probe that did not run is not a clean ceiling: the verdict would otherwise clear the product on evidence nobody collected.
	if len(unknown) > 0 {
		s.Set("clean_failure", "inconclusive: "+strings.Join(unknown, "; "))
		return fmt.Errorf("the ceiling could not be shown to be clean: %s", strings.Join(unknown, "; "))
	}
	s.Set("clean_failure", "yes")
	return nil
}

func endpointPodByID(ctx context.Context, kube *k8s.Kubectl, endpointID string) (string, error) {
	pods, err := kube.ListPods(ctx)
	if err != nil {
		return "", fmt.Errorf("list pods: %w", err)
	}
	want := "yb-ep-" + endpointID
	for _, p := range pods {
		if strings.HasPrefix(p, want) {
			return p, nil
		}
	}
	return "", fmt.Errorf("no pod matches endpoint %s", endpointID)
}
