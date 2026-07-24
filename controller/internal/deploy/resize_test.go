package deploy

import (
	"context"
	"testing"
)

func TestClassifyResize(t *testing.T) {
	// The classifier is always asked "is the endpoint container at 4 CPU / 8Gi?".
	const wantCPU, wantMem, container = "4", "8Gi", "endpoint"
	matched := `{"status":{"containerStatuses":[{"name":"endpoint","resources":{"requests":{"cpu":"4","memory":"8Gi"}}}]}}`
	stale := `{"status":{"containerStatuses":[{"name":"endpoint","resources":{"requests":{"cpu":"2","memory":"4Gi"}}}]}}`

	tests := []struct {
		name     string
		podJSON  string
		want     ResizeOutcome
		wantMsgN bool // whether a non-empty message is expected
	}{
		{
			name:    "no condition + allocated matches request => completed",
			podJSON: matched,
			want:    ResizeCompleted,
		},
		{
			name:    "no condition but allocated still old => pending (not applied yet)",
			podJSON: stale,
			want:    ResizePending,
		},
		{
			name:    "no condition, container status absent yet => pending (still settling)",
			podJSON: `{"status":{"conditions":[{"type":"Ready","status":"True"}]}}`,
			want:    ResizePending,
		},
		{
			name:     "pending+infeasible => infeasible (even if allocated not yet updated)",
			podJSON:  `{"status":{"conditions":[{"type":"PodResizePending","status":"True","reason":"Infeasible","message":"Node didn't have enough capacity"}]}}`,
			want:     ResizeInfeasible,
			wantMsgN: true,
		},
		{
			name:    "pending+deferred => pending",
			podJSON: `{"status":{"conditions":[{"type":"PodResizePending","status":"True","reason":"Deferred"}]}}`,
			want:    ResizePending,
		},
		{
			name:    "in progress => pending",
			podJSON: `{"status":{"conditions":[{"type":"PodResizeInProgress","status":"True"}]}}`,
			want:    ResizePending,
		},
		{
			name:    "a False pending condition is ignored; allocated matches => completed",
			podJSON: `{"status":{"conditions":[{"type":"PodResizePending","status":"False","reason":"Infeasible"}],"containerStatuses":[{"name":"endpoint","resources":{"requests":{"cpu":"4","memory":"8Gi"}}}]}}`,
			want:    ResizeCompleted,
		},
		{
			name:    "unparseable body => pending (best effort)",
			podJSON: `not json`,
			want:    ResizePending,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyResize([]byte(tt.podJSON), wantCPU, wantMem, container)
			if got.Outcome != tt.want {
				t.Errorf("outcome = %q, want %q", got.Outcome, tt.want)
			}
			if tt.wantMsgN && got.Message == "" {
				t.Error("expected a non-empty message")
			}
		})
	}
}

func TestDockerResizeUnsupported(t *testing.T) {
	d := &DockerDeployer{}
	res, err := d.ResizeEndpoint(context.Background(), ResizeSpec{Name: "yb-ep-x", CPU: "4", Memory: "8Gi"})
	if err != nil {
		t.Fatalf("ResizeEndpoint returned error, want a clean unsupported result: %v", err)
	}
	if res.Outcome != ResizeUnsupported {
		t.Errorf("outcome = %q, want %q", res.Outcome, ResizeUnsupported)
	}
}
