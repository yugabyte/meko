package deploy

// Why a failed endpoint deploy says more than `context deadline exceeded` (#635):
// helm --wait reports that same sentence whether the pod was unschedulable, in
// ImagePullBackOff, crash-looping or merely slow, and the reason was on the pod
// the whole time. See controller/architecture/ac-k8s-deploy.md.
//
// The reading has to happen DURING the wait: helm tears the Deployment and pod
// down when it gives up (runbooks/controller_runbook.md, "A `helm --wait` pod is
// ephemeral"), so by the time the error is assembled there is usually nothing
// left to read.
//
// All of it is best-effort. A diagnosis that cannot be made costs the deploy
// nothing — the helm error is still returned, just unadorned.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// diagnoseInterval paces the snapshots taken while helm --wait blocks. The
// states being caught (unschedulable, image pull back-off, crash loop) persist
// for minutes, so this is about not missing the teardown rather than about
// resolution — and it runs against the apiserver for the whole deploy timeout.
const diagnoseInterval = 15 * time.Second

// diagnoseMaxEvents caps how many distinct events are quoted. Enough to carry
// the scheduler's, the autoscaler's and the kubelet's messages together, short
// of pasting the pod's whole event history into a task row.
const diagnoseMaxEvents = 3

// waitingReasonsBenign are the container waiting reasons that mean "still
// working on it". Everything else the kubelet puts there is a failure worth
// reporting — ImagePullBackOff, ErrImagePull, CreateContainerError,
// CreateContainerConfigError, InvalidImageName, and whatever a future kubelet
// adds, which is why this is a deny-list rather than a list of the known-bad.
var waitingReasonsBenign = map[string]bool{
	"ContainerCreating": true,
	"PodInitializing":   true,
}

// podDiagnosis is what the pod said about itself. conclusive marks the cases
// where the pod named its own failure, which decides which of two snapshots to
// keep. wantEvents overrides the "conclusive needs no events" shortcut for the
// one case where the pod's own message is not the whole answer.
type podDiagnosis struct {
	summary    string
	conclusive bool
	wantEvents bool
}

// diagnosePod turns a Pod object into the sentence an operator can act on.
//
// The order is by how specific the answer is, not by how the pod progresses: a
// pod that never scheduled has a scheduler message and nothing else, one whose
// container will not start has the kubelet's, and one that started and died has
// an exit code. The first of those that is present is the real cause; a later
// one, if it is set at all, is a consequence.
func diagnosePod(podBody []byte) podDiagnosis {
	var p struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Phase      string `json:"phase"`
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
			ContainerStatuses []struct {
				Name         string `json:"name"`
				Image        string `json:"image"`
				Ready        bool   `json:"ready"`
				RestartCount int    `json:"restartCount"`
				State        struct {
					Waiting *struct {
						Reason  string `json:"reason"`
						Message string `json:"message"`
					} `json:"waiting"`
				} `json:"state"`
				LastState struct {
					Terminated *struct {
						Reason   string `json:"reason"`
						ExitCode int    `json:"exitCode"`
						Message  string `json:"message"`
					} `json:"terminated"`
				} `json:"lastState"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := json.Unmarshal(podBody, &p); err != nil {
		return podDiagnosis{}
	}
	pod := p.Metadata.Name
	if pod == "" {
		pod = "endpoint pod"
	}

	for _, cond := range p.Status.Conditions {
		if cond.Type != "PodScheduled" || cond.Status != "False" {
			continue
		}
		// The scheduler's own message ("0/14 nodes are available: 13 Insufficient
		// ephemeral-storage, …") is the whole diagnosis where it is set; the reason
		// alone ("Unschedulable") is not, so it is only the fallback.
		detail := strings.TrimSpace(cond.Message)
		if detail == "" {
			detail = cond.Reason
		}
		// Always ask for events on a scheduling failure, conclusive or not. The
		// scheduler says which nodes it rejected ("0/14 nodes are available: …
		// Insufficient ephemeral-storage"); only the cluster-autoscaler says why no
		// node can be ADDED ("NotTriggerScaleUp … 9 exceeded quota: cluster-wide,
		// resources: cpu"). The second is the actionable one, and it is the reason
		// #635's reporter raised the timeout 20m→35m and failed again at 35.5.
		if detail == "" {
			return podDiagnosis{summary: fmt.Sprintf("pod %s could not be scheduled", pod), wantEvents: true}
		}
		return podDiagnosis{
			summary:    fmt.Sprintf("pod %s could not be scheduled: %s", pod, detail),
			conclusive: true,
			wantEvents: true,
		}
	}

	for _, cs := range p.Status.ContainerStatuses {
		w := cs.State.Waiting
		if w == nil || waitingReasonsBenign[w.Reason] {
			continue
		}
		// The image is named explicitly: an ImagePullBackOff message does not always
		// carry it, and "which tag" is the first question a pull failure raises.
		msg := fmt.Sprintf("container %s (image %s) cannot start: %s", cs.Name, cs.Image, w.Reason)
		if detail := strings.TrimSpace(w.Message); detail != "" {
			msg += ": " + detail
		}
		return podDiagnosis{summary: msg, conclusive: true}
	}

	for _, cs := range p.Status.ContainerStatuses {
		t := cs.LastState.Terminated
		if t == nil {
			continue
		}
		msg := fmt.Sprintf("container %s exited with code %d", cs.Name, t.ExitCode)
		if t.Reason != "" {
			msg += " (" + t.Reason + ")"
		}
		msg += fmt.Sprintf(" and has restarted %d time(s)", cs.RestartCount)
		if detail := strings.TrimSpace(t.Message); detail != "" {
			msg += ": " + detail
		}
		return podDiagnosis{summary: msg, conclusive: true}
	}

	// Nothing failed outright — the pod is simply not ready yet. Say exactly that,
	// with the state it is in, which still beats a bare timeout.
	phase := p.Status.Phase
	if phase == "" {
		phase = "Unknown"
	}
	if phase == "Pending" && len(p.Status.Conditions) == 0 {
		return podDiagnosis{summary: fmt.Sprintf("pod %s is Pending with no scheduling decision yet", pod)}
	}
	for _, cs := range p.Status.ContainerStatuses {
		if !cs.Ready {
			return podDiagnosis{summary: fmt.Sprintf("pod %s is %s and container %s is running but not ready (restarts: %d)",
				pod, phase, cs.Name, cs.RestartCount)}
		}
	}
	return podDiagnosis{summary: fmt.Sprintf("pod %s is %s and reports no failure", pod, phase)}
}

// normalEventReasons are the Normal-type events worth quoting. Type is not a
// reliable guide to importance here: cluster-autoscaler records the reason a pod
// can never be placed — "NotTriggerScaleUp: … 9 exceeded quota: cluster-wide,
// resources: cpu" — as NORMAL, and that single line is the whole diagnosis
// #635's reporter spent two runs and a timeout bump not having. A Warning-only
// filter drops exactly the event the issue was filed about.
var normalEventReasons = map[string]bool{
	"NotTriggerScaleUp": true,
}

// diagnoseEvents quotes the pod's most recent distinct events, newest first:
// every Warning, plus the Normal reasons above.
//
// Deduplicated because the kubelet and the scheduler repeat one message for as
// long as the condition holds, and N copies of "FailedScheduling" say no more
// than one.
func diagnoseEvents(eventsBody []byte, limit int) string {
	var out struct {
		Items []struct {
			Type          string `json:"type"`
			Reason        string `json:"reason"`
			Message       string `json:"message"`
			LastTimestamp string `json:"lastTimestamp"`
			EventTime     string `json:"eventTime"`
		} `json:"items"`
	}
	if err := json.Unmarshal(eventsBody, &out); err != nil {
		return ""
	}
	type ev struct{ at, text string }
	var evs []ev
	for _, it := range out.Items {
		if strings.TrimSpace(it.Message) == "" {
			continue
		}
		if it.Type != "Warning" && !normalEventReasons[it.Reason] {
			continue
		}
		at := it.LastTimestamp
		if at == "" {
			at = it.EventTime
		}
		evs = append(evs, ev{at: at, text: it.Reason + ": " + strings.TrimSpace(it.Message)})
	}
	// RFC3339 sorts lexicographically, so no time parsing is needed to order
	// these; an event with no timestamp at all sorts last, which is where an
	// unorderable event belongs.
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].at > evs[j].at })

	seen := make(map[string]bool, len(evs))
	var picked []string
	for _, e := range evs {
		if seen[e.text] {
			continue
		}
		seen[e.text] = true
		picked = append(picked, e.text)
		if len(picked) == limit {
			break
		}
	}
	return strings.Join(picked, "; ")
}

// diagnoseRelease reads the release's pod and reports what it found. An empty
// diagnosis means the pod could not be read at all (gone, or not in-cluster).
//
// withEvents adds the pod's Warning events when the pod alone is inconclusive,
// and only the failure path passes it. A pod that is merely still starting is
// inconclusive too, so asking on every snapshot would put a namespace-wide event
// LIST (the involvedObject fieldSelector is unindexed) on every tick of every
// healthy deploy — tens per create, for an answer nobody reads.
func (k *KubernetesDeployer) diagnoseRelease(ctx context.Context, release string, withEvents bool) podDiagnosis {
	podBody, ok := k.releasePod(ctx, release)
	if !ok {
		return podDiagnosis{}
	}
	diag := diagnosePod(podBody)
	if !withEvents || diag.summary == "" || (diag.conclusive && !diag.wantEvents) {
		return diag
	}
	name := podObjectName(podBody)
	if name == "" {
		return diag
	}
	if warn := k.podWarnings(ctx, name); warn != "" {
		return podDiagnosis{summary: diag.summary + "; recent events: " + warn, conclusive: true}
	}
	return diag
}

// releasePod returns the raw Pod object for a release, found by the same
// instance label podName uses.
func (k *KubernetesDeployer) releasePod(ctx context.Context, release string) ([]byte, bool) {
	q := url.Values{"labelSelector": {"app.kubernetes.io/instance=" + release}, "limit": {"1"}}
	body, ok := k.k8sAPIGet(ctx, fmt.Sprintf("/api/v1/namespaces/%s/pods?%s", k.cfg.Namespace, q.Encode()))
	if !ok {
		return nil, false
	}
	var out struct {
		Items []json.RawMessage `json:"items"`
	}
	if json.Unmarshal(body, &out) != nil || len(out.Items) == 0 {
		return nil, false
	}
	return out.Items[0], true
}

// podObjectName reads .metadata.name off a Pod object.
func podObjectName(podBody []byte) string {
	var p struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if json.Unmarshal(podBody, &p) != nil {
		return ""
	}
	return p.Metadata.Name
}

// podWarnings summarizes the recent Warning events on one pod.
func (k *KubernetesDeployer) podWarnings(ctx context.Context, pod string) string {
	q := url.Values{"fieldSelector": {"involvedObject.name=" + pod}}
	body, ok := k.k8sAPIGet(ctx, fmt.Sprintf("/api/v1/namespaces/%s/events?%s", k.cfg.Namespace, q.Encode()))
	if !ok {
		return ""
	}
	return diagnoseEvents(body, diagnoseMaxEvents)
}

// deployWatcher keeps the best diagnosis seen while a deploy was in flight, so
// the pod's reason survives helm tearing the pod down.
type deployWatcher struct {
	mu   sync.Mutex
	best podDiagnosis
	stop context.CancelFunc
	done chan struct{}
}

// watchDeploy starts snapshotting the release's pod until the returned
// watcher is closed. The caller must close it; closing waits for the snapshot
// goroutine so it cannot outlive the deploy.
func (k *KubernetesDeployer) watchDeploy(ctx context.Context, release string) *deployWatcher {
	ctx, cancel := context.WithCancel(ctx)
	w := &deployWatcher{stop: cancel, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(diagnoseInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			w.record(k.diagnoseRelease(ctx, release, false))
		}
	}()
	return w
}

// record keeps a snapshot when it is better than the one held: a conclusive
// diagnosis displaces an inconclusive one, and a later snapshot of equal
// standing displaces an earlier one (a pod that went from Pending to
// CrashLoopBackOff failed for the second reason).
func (w *deployWatcher) record(d podDiagnosis) {
	if d.summary == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.best.conclusive && !d.conclusive {
		return
	}
	w.best = d
}

func (w *deployWatcher) latest() podDiagnosis {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.best
}

// close stops the watcher and waits for its goroutine.
func (w *deployWatcher) close() {
	w.stop()
	<-w.done
}

// explainDeployFailure puts the workload's own reason in front of helm's.
//
// A fresh read wins when it is conclusive — the pod may still be there, and it
// is the current state. Otherwise the watcher's snapshot from during the wait
// is what is left, because helm has since removed the pod.
//
// The diagnosis leads the whole error, with no "helm deploy <release>" ahead of
// it: the task error is read truncated in a table cell, so the first clause has
// to be the cause. Nothing is lost — the pod names the release and so does
// helm's own message at the tail.
func (k *KubernetesDeployer) explainDeployFailure(ctx context.Context, release string, w *deployWatcher, err error) error {
	reason := k.diagnoseRelease(ctx, release, true)
	if !reason.conclusive {
		if snap := w.latest(); snap.conclusive || reason.summary == "" {
			reason = snap
		}
	}
	if reason.summary == "" {
		return fmt.Errorf("endpoint pod for %s never became ready and its state could not be read (pod already removed, or the controller is not running in-cluster): %w", release, err)
	}
	k.logger.Warn("endpoint deploy failed", "endpoint", release, "diagnosis", reason.summary)
	return fmt.Errorf("%s: %w", reason.summary, err)
}
