package deploy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReleaseHealth(t *testing.T) {
	const (
		ready = `{"metadata":{"name":"new"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}`
		// The meko#1746 case: the pod runs, a sidecar crash-loops, so the pod is
		// not Ready and the reason names the container.
		crashLooping = `{"metadata":{"name":"new"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"False"}],
			"containerStatuses":[{"name":"documentdb-gateway","image":"gw:1","state":{"waiting":{"reason":"CrashLoopBackOff","message":"back-off 2m40s"}}}]}}`
		terminatingReady = `{"metadata":{"name":"old","deletionTimestamp":"2026-10-06T16:28:00Z"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}`
		noConditions     = `{"metadata":{"name":"new"},"status":{"phase":"Running"}}`
		evicted          = `{"metadata":{"name":"old"},"status":{"phase":"Failed","reason":"Evicted","conditions":[{"type":"Ready","status":"False"}]}}`
	)
	tests := []struct {
		name       string
		pods       []string
		wantReady  bool
		wantReason string
	}{
		{name: "ready pod", pods: []string{ready}, wantReady: true},
		{name: "no pod", wantReason: "no endpoint pod exists"},
		{name: "crash-looping container is named", pods: []string{crashLooping}, wantReason: "container documentdb-gateway (image gw:1) cannot start: CrashLoopBackOff"},
		// Recreate leaves the old pod terminating while the new one starts; the old
		// one is not serving, whatever its condition still says.
		{name: "terminating pod does not mask its replacement", pods: []string{terminatingReady, crashLooping}, wantReason: "CrashLoopBackOff"},
		{name: "only a terminating pod", pods: []string{terminatingReady}, wantReason: "terminating"},
		// An evicted pod stays listed, phase Failed, beside the replacement.
		{name: "evicted pod does not supply the reason", pods: []string{evicted, crashLooping}, wantReason: "CrashLoopBackOff"},
		{name: "only an evicted pod", pods: []string{evicted}, wantReason: "has exited"},
		{name: "no Ready condition is not ready", pods: []string{noConditions}, wantReason: "reports no failure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var pods []json.RawMessage
			for _, p := range tt.pods {
				pods = append(pods, json.RawMessage(p))
			}
			got := releaseHealth(pods)
			if got.Ready != tt.wantReady {
				t.Fatalf("Ready = %v, want %v (reason %q)", got.Ready, tt.wantReady, got.Reason)
			}
			if tt.wantReady && got.Reason != "" {
				t.Errorf("Reason = %q on a ready endpoint, want empty", got.Reason)
			}
			if !strings.Contains(got.Reason, tt.wantReason) {
				t.Errorf("Reason = %q, want it to contain %q", got.Reason, tt.wantReason)
			}
		})
	}
}

func TestPodInstance(t *testing.T) {
	pod := `{"metadata":{"labels":{"app.kubernetes.io/instance":"yb-ep-1"}}}`
	if got := podInstance([]byte(pod)); got != "yb-ep-1" {
		t.Errorf("podInstance = %q, want yb-ep-1", got)
	}
}
