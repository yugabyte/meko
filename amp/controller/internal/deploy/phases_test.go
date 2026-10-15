package deploy

import (
	"testing"
	"time"
)

func TestPodReadyTimes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		wantOK bool
		want   time.Duration
	}{
		{
			name: "ready",
			body: `{"metadata":{"creationTimestamp":"2026-10-15T10:00:00Z"},"status":{"conditions":[
				{"type":"PodScheduled","status":"True","lastTransitionTime":"2026-10-15T10:00:01Z"},
				{"type":"Ready","status":"True","lastTransitionTime":"2026-10-15T10:00:12Z"}]}}`,
			wantOK: true,
			want:   12 * time.Second,
		},
		{
			name: "not ready",
			body: `{"metadata":{"creationTimestamp":"2026-10-15T10:00:00Z"},"status":{"conditions":[
				{"type":"Ready","status":"False","lastTransitionTime":"2026-10-15T10:00:12Z"}]}}`,
		},
		{name: "no creation time", body: `{"status":{"conditions":[{"type":"Ready","status":"True","lastTransitionTime":"2026-10-15T10:00:12Z"}]}}`},
		{name: "not json", body: `nope`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created, ready, ok := podReadyTimes([]byte(tc.body))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && ready.Sub(created) != tc.want {
				t.Errorf("created->ready = %v, want %v", ready.Sub(created), tc.want)
			}
		})
	}
}
