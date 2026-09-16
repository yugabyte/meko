package deploy

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/yugabyte/amp/controller/internal/helm"
)

// The failure modes #635 has to tell apart. Each is the pod state the
// controller would have found while helm --wait was still blocking.
func TestDiagnosePod(t *testing.T) {
	tests := []struct {
		name       string
		podJSON    string
		wantSubstr []string
		conclusive bool
	}{
		{
			// The reported case: a 4-vCPU endpoint asking 80 GiB ephemeral-storage on a
			// pool whose largest schedulable node has 43.9 GiB.
			name: "unschedulable carries the scheduler's own message",
			podJSON: `{"metadata":{"name":"yb-ep-abc-7d9"},"status":{"phase":"Pending","conditions":[
				{"type":"PodScheduled","status":"False","reason":"Unschedulable",
				 "message":"0/14 nodes are available: 1 node(s) were unschedulable, 13 Insufficient ephemeral-storage."}]}}`,
			wantSubstr: []string{"yb-ep-abc-7d9", "could not be scheduled", "Insufficient ephemeral-storage"},
			conclusive: true,
		},
		{
			name: "unschedulable with no message falls back to the reason",
			podJSON: `{"metadata":{"name":"yb-ep-abc-7d9"},"status":{"phase":"Pending","conditions":[
				{"type":"PodScheduled","status":"False","reason":"SchedulerError"}]}}`,
			wantSubstr: []string{"could not be scheduled", "SchedulerError"},
			conclusive: true,
		},
		{
			name: "image pull failure names the image and the pull error",
			podJSON: `{"metadata":{"name":"yb-ep-abc-7d9"},"status":{"phase":"Pending","conditions":[
				{"type":"PodScheduled","status":"True"}],"containerStatuses":[
				{"name":"endpoint","image":"repo/endpoint:v9","state":{"waiting":
				 {"reason":"ImagePullBackOff","message":"Back-off pulling image \"repo/endpoint:v9\""}}}]}}`,
			wantSubstr: []string{"endpoint", "repo/endpoint:v9", "ImagePullBackOff", "Back-off pulling"},
			conclusive: true,
		},
		{
			name: "a crash-looping supervisor reports its exit code and restarts",
			podJSON: `{"metadata":{"name":"yb-ep-abc-7d9"},"status":{"phase":"Running","containerStatuses":[
				{"name":"endpoint","image":"repo/endpoint:v9","ready":false,"restartCount":6,
				 "state":{"waiting":{"reason":"CrashLoopBackOff","message":"back-off 5m0s restarting failed container"}},
				 "lastState":{"terminated":{"reason":"Error","exitCode":1}}}]}}`,
			// CrashLoopBackOff is a waiting reason, so it is reported before the
			// terminated state — both name the same failure, and the waiting one is
			// what `kubectl get pods` shows.
			wantSubstr: []string{"CrashLoopBackOff"},
			conclusive: true,
		},
		{
			name: "an OOMKilled container that is not waiting reports the terminated state",
			podJSON: `{"metadata":{"name":"yb-ep-abc-7d9"},"status":{"phase":"Running","containerStatuses":[
				{"name":"endpoint","image":"repo/endpoint:v9","ready":false,"restartCount":2,
				 "lastState":{"terminated":{"reason":"OOMKilled","exitCode":137}}}]}}`,
			wantSubstr: []string{"exited with code 137", "OOMKilled", "restarted 2 time(s)"},
			conclusive: true,
		},
		{
			// Inconclusive: the pod is doing nothing wrong, it is just slow. Worth
			// saying, but it is what earns the extra events lookup.
			name: "running but not ready is reported and left inconclusive",
			podJSON: `{"metadata":{"name":"yb-ep-abc-7d9"},"status":{"phase":"Running","conditions":[
				{"type":"PodScheduled","status":"True"}],"containerStatuses":[
				{"name":"endpoint","image":"repo/endpoint:v9","ready":false,"restartCount":0,
				 "state":{"running":{}}}]}}`,
			wantSubstr: []string{"Running", "not ready", "restarts: 0"},
		},
		{
			name:       "pending with no conditions at all says so",
			podJSON:    `{"metadata":{"name":"yb-ep-abc-7d9"},"status":{"phase":"Pending"}}`,
			wantSubstr: []string{"Pending", "no scheduling decision"},
		},
		{
			name: "ContainerCreating is not a failure",
			podJSON: `{"metadata":{"name":"yb-ep-abc-7d9"},"status":{"phase":"Pending","conditions":[
				{"type":"PodScheduled","status":"True"}],"containerStatuses":[
				{"name":"endpoint","image":"repo/endpoint:v9","state":{"waiting":{"reason":"ContainerCreating"}}}]}}`,
			wantSubstr: []string{"not ready"},
		},
		{
			name: "a True PodScheduled condition is not a scheduling failure",
			podJSON: `{"metadata":{"name":"yb-ep-abc-7d9"},"status":{"phase":"Running","conditions":[
				{"type":"PodScheduled","status":"True"}],"containerStatuses":[
				{"name":"endpoint","image":"repo/endpoint:v9","ready":true}]}}`,
			wantSubstr: []string{"reports no failure"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := diagnosePod([]byte(tt.podJSON))
			for _, want := range tt.wantSubstr {
				if !strings.Contains(got.summary, want) {
					t.Errorf("diagnosePod() = %q, want it to contain %q", got.summary, want)
				}
			}
			if got.conclusive != tt.conclusive {
				t.Errorf("diagnosePod() conclusive = %v, want %v (summary %q)", got.conclusive, tt.conclusive, got.summary)
			}
		})
	}
}

// An unreadable or empty body must produce no diagnosis at all, so the caller
// falls back to helm's error rather than inventing a cause.
func TestDiagnosePodNoAnswer(t *testing.T) {
	for _, body := range []string{"not json", ""} {
		if got := diagnosePod([]byte(body)); got.summary != "" {
			t.Errorf("diagnosePod(%q) = %q, want no diagnosis", body, got.summary)
		}
	}
}

func TestDiagnoseEvents(t *testing.T) {
	body := `{"items":[
		{"type":"Normal","reason":"Scheduled","message":"assigned to node-1","lastTimestamp":"2026-09-15T10:00:00Z"},
		{"type":"Warning","reason":"FailedScheduling","message":"0/14 nodes are available","lastTimestamp":"2026-09-15T10:01:00Z"},
		{"type":"Warning","reason":"FailedScheduling","message":"0/14 nodes are available","lastTimestamp":"2026-09-15T10:02:00Z"},
		{"type":"Warning","reason":"FailedMount","message":"timed out waiting for the condition","lastTimestamp":"2026-09-15T10:03:00Z"}]}`

	got := diagnoseEvents([]byte(body), 3)
	want := "FailedMount: timed out waiting for the condition; FailedScheduling: 0/14 nodes are available"
	if got != want {
		t.Errorf("diagnoseEvents() = %q, want %q", got, want)
	}
}

func TestDiagnoseEventsLimit(t *testing.T) {
	body := `{"items":[
		{"type":"Warning","reason":"A","message":"a","lastTimestamp":"2026-09-15T10:01:00Z"},
		{"type":"Warning","reason":"B","message":"b","lastTimestamp":"2026-09-15T10:02:00Z"},
		{"type":"Warning","reason":"C","message":"c","lastTimestamp":"2026-09-15T10:03:00Z"}]}`

	if got, want := diagnoseEvents([]byte(body), 2), "C: c; B: b"; got != want {
		t.Errorf("diagnoseEvents(limit 2) = %q, want %q", got, want)
	}
	if got := diagnoseEvents([]byte(`{"items":[{"type":"Normal","reason":"Pulled","message":"ok"}]}`), 3); got != "" {
		t.Errorf("diagnoseEvents() over Normal events = %q, want empty", got)
	}
	if got := diagnoseEvents([]byte("not json"), 3); got != "" {
		t.Errorf("diagnoseEvents(garbage) = %q, want empty", got)
	}
}

// The watcher holds the reason the pod gave while it still existed, because
// helm removes the pod when --wait gives up.
func TestDeployWatcherRecord(t *testing.T) {
	w := &deployWatcher{}
	if got := w.latest(); got.summary != "" {
		t.Errorf("fresh watcher = %q, want empty", got.summary)
	}

	w.record(podDiagnosis{summary: "pending"})
	w.record(podDiagnosis{})
	if got := w.latest(); got.summary != "pending" {
		t.Errorf("after an empty snapshot = %q, want %q", got.summary, "pending")
	}

	w.record(podDiagnosis{summary: "unschedulable", conclusive: true})
	w.record(podDiagnosis{summary: "still pending"})
	if got := w.latest(); got.summary != "unschedulable" {
		t.Errorf("an inconclusive snapshot displaced a conclusive one: got %q", got.summary)
	}

	w.record(podDiagnosis{summary: "crash-looping", conclusive: true})
	if got := w.latest(); got.summary != "crash-looping" {
		t.Errorf("a later conclusive snapshot did not win: got %q", got.summary)
	}
}

// The error an operator reads off /tasks. Out of cluster nothing can be read
// live, which is also the shape of the real failure that matters: helm has
// removed the pod, and the watcher's snapshot is all that is left.
func TestExplainDeployFailure(t *testing.T) {
	k := NewKubernetesDeployer(KubernetesConfig{Namespace: "amp-dev"}, slog.New(slog.DiscardHandler))
	helmErr := errors.New("release yb-ep-abc did not become ready within 10m0s: " +
		"helm upgrade --install yb-ep-abc: exit status 1: Error: context deadline exceeded")

	w := &deployWatcher{}
	w.record(podDiagnosis{
		summary:    "pod yb-ep-abc-7d9 could not be scheduled: 0/14 nodes are available: 13 Insufficient ephemeral-storage.",
		conclusive: true,
	})
	got := k.explainDeployFailure(t.Context(), "yb-ep-abc", w, helmErr).Error()
	// The cause leads: the task error is read truncated in a table cell.
	if !strings.HasPrefix(got, "pod yb-ep-abc-7d9 could not be scheduled: ") {
		t.Errorf("explainDeployFailure() = %q, want the diagnosis first", got)
	}
	if !strings.Contains(got, "context deadline exceeded") {
		t.Errorf("explainDeployFailure() = %q, want helm's error kept at the tail", got)
	}

	// Nothing known: say that plainly rather than pretending, and still name the
	// release and helm's failure.
	bare := k.explainDeployFailure(t.Context(), "yb-ep-abc", &deployWatcher{}, helmErr).Error()
	for _, want := range []string{"yb-ep-abc", "state could not be read", "context deadline exceeded"} {
		if !strings.Contains(bare, want) {
			t.Errorf("explainDeployFailure() with no diagnosis = %q, want it to contain %q", bare, want)
		}
	}
}

// Only a wait expiry gets a pod diagnosis. A chart render refusal never creates
// a pod, and leading it with "the pod could not be read" would bury the one
// message that does explain it — the failure this PR exists to prevent.
func TestDeployFailureEnrichmentIsGatedOnTheWaitExpiring(t *testing.T) {
	renderErr := fmt.Errorf("helm upgrade --install yb-ep-abc: exit status 1: "+
		"Error: execution error at (yb-amp-endpoint/templates/service-lb.yaml:28:4): "+
		"service.connectivity=nlb_internal needs service.cloud to be one of aws, gcp, azure: %w",
		errors.New("exit status 1"))
	if errors.Is(renderErr, helm.ErrWaitTimeout) {
		t.Fatal("a chart render failure must not look like a wait expiry")
	}

	waitErr := fmt.Errorf("release yb-ep-abc did not become ready within 10m0s (%w): exit status 1",
		helm.ErrWaitTimeout)
	if !errors.Is(waitErr, helm.ErrWaitTimeout) {
		t.Fatal("a wait expiry must be recognizable by the deployer")
	}
}

// The event #635 was filed about. cluster-autoscaler records "this pod can never
// be placed" as a NORMAL event, so a Warning-only filter drops the one line that
// explains the failure.
func TestDiagnoseEventsKeepsNotTriggerScaleUp(t *testing.T) {
	body := `{"items":[
		{"type":"Normal","reason":"NotTriggerScaleUp","message":"Pod didn't trigger scale-up: 9 exceeded quota: \"cluster-wide\", resources: cpu","lastTimestamp":"2026-09-15T10:02:00Z"},
		{"type":"Normal","reason":"Scheduled","message":"assigned to node-1","lastTimestamp":"2026-09-15T10:01:00Z"},
		{"type":"Warning","reason":"FailedScheduling","message":"0/14 nodes are available","lastTimestamp":"2026-09-15T10:03:00Z"}]}`

	got := diagnoseEvents([]byte(body), 3)
	if !strings.Contains(got, "NotTriggerScaleUp") || !strings.Contains(got, "exceeded quota") {
		t.Errorf("diagnoseEvents() = %q, want the autoscaler's quota reason", got)
	}
	if strings.Contains(got, "Scheduled: assigned") {
		t.Errorf("diagnoseEvents() = %q, want ordinary Normal events left out", got)
	}
}

// An unschedulable pod is conclusive, but the scheduler says only which nodes it
// rejected — the autoscaler says why no node can be added. So a scheduling
// failure asks for events anyway.
func TestSchedulingFailureAsksForEvents(t *testing.T) {
	podJSON := `{"metadata":{"name":"yb-ep-abc-7d9"},"status":{"phase":"Pending","conditions":[
		{"type":"PodScheduled","status":"False","reason":"Unschedulable",
		 "message":"0/14 nodes are available: 13 Insufficient ephemeral-storage."}]}}`

	got := diagnosePod([]byte(podJSON))
	if !got.conclusive {
		t.Error("an unschedulable pod should still be a conclusive diagnosis")
	}
	if !got.wantEvents {
		t.Error("a scheduling failure must ask for events: the autoscaler's reason lives there, not on the pod")
	}

	// Everything else that is conclusive is answered by the pod alone.
	crash := diagnosePod([]byte(`{"metadata":{"name":"p"},"status":{"phase":"Running","containerStatuses":[
		{"name":"endpoint","image":"repo/e:v9","lastState":{"terminated":{"reason":"OOMKilled","exitCode":137}}}]}}`))
	if crash.wantEvents {
		t.Error("a container that named its own exit code does not need an events LIST")
	}
}
