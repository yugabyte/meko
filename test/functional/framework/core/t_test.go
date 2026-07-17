package core

import (
	"context"
	"testing"
)

// TestResourceSuffixSameSeedDiffers is the same-seed collision guard: two test
// executions replaying the identical --seed must still get DIFFERENT resource
// suffixes, so their infra tags/namespaces/SQL identifiers never clash and two
// agents can't reap each other's environments.
func TestResourceSuffixSameSeedDiffers(t *testing.T) {
	seed := NewRunSeed(0x1234, true).ForTest("same/test")
	a := newT(context.Background(), nil, seed, nil, nil, &Result{}, nil)
	b := newT(context.Background(), nil, seed, nil, nil, &Result{}, nil)

	if a.Seed.Value() != b.Seed.Value() {
		t.Fatalf("precondition: seeds differ (%d vs %d), want identical", a.Seed.Value(), b.Seed.Value())
	}
	if a.ResourceSuffix() == b.ResourceSuffix() {
		t.Fatalf("same-seed suffixes collided: both %q (nonce not applied)", a.ResourceSuffix())
	}
}

// TestStepMetricRetentionCapped: past the configured cap, Steps/Metrics
// retention is bounded (keeps the last N) and the dropped count is recorded, so
// a long soak emitting per-cycle events can't grow the Result unbounded.
func TestStepMetricRetentionCapped(t *testing.T) {
	const capN = 5
	const emitN = 20
	cfg, err := LoadConfig(nil, nil, []string{
		"report.max_steps_retained=int:5",
		"report.max_metrics_retained=int:5",
	})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	res := &Result{}
	seed := NewRunSeed(1, true).ForTest("cap/test")
	tt := newT(context.Background(), nil, seed, cfg, nil, res, nil)

	for i := 0; i < emitN; i++ {
		if err := tt.Step("step", func(*StepHandle) error { return nil }); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		tt.Metric("m", float64(i), "count")
	}

	if len(res.Steps) != capN {
		t.Errorf("retained steps = %d, want %d (cap)", len(res.Steps), capN)
	}
	if len(res.Metrics) != capN {
		t.Errorf("retained metrics = %d, want %d (cap)", len(res.Metrics), capN)
	}
	if res.StepsDropped != emitN-capN {
		t.Errorf("steps dropped = %d, want %d", res.StepsDropped, emitN-capN)
	}
	if res.MetricsDropped != emitN-capN {
		t.Errorf("metrics dropped = %d, want %d", res.MetricsDropped, emitN-capN)
	}
	// The retained metrics must be the LAST N (most recent), not the first N.
	if res.Metrics[len(res.Metrics)-1].Value != float64(emitN-1) {
		t.Errorf("newest retained metric = %v, want %v (last-N retention)", res.Metrics[len(res.Metrics)-1].Value, float64(emitN-1))
	}
}

// TestAttachBytesRecordsMetadataAndEmitsContent: AttachBytes records the
// attachment metadata on the result (name/kind/size) exactly like Attach AND
// emits an EventAttachment carrying the raw bytes for a persisting sink.
func TestAttachBytesRecordsMetadataAndEmitsContent(t *testing.T) {
	res := &Result{}
	cap := &captureEmitter{}
	seed := NewRunSeed(1, true).ForTest("attach/bytes")
	tt := newT(context.Background(), nil, seed, nil, nil, res, cap)

	content := []byte("controller crash backtrace")
	tt.AttachBytes("controller.log", "log", content)

	if len(res.Attachments) != 1 {
		t.Fatalf("recorded attachments = %d, want 1", len(res.Attachments))
	}
	a := res.Attachments[0]
	if a.Name != "controller.log" || a.Kind != "log" || a.Size != int64(len(content)) {
		t.Errorf("recorded attachment = %+v, want controller.log/log/%d", a, len(content))
	}
	if len(cap.events) != 1 || cap.events[0].Kind != EventAttachment {
		t.Fatalf("emitted events = %+v, want one EventAttachment", cap.events)
	}
	if string(cap.events[0].Content) != string(content) {
		t.Errorf("emitted Content = %q, want %q", cap.events[0].Content, content)
	}
}

// TestAttachMetadataOnlyEmitsNoContent: the metadata-only Attach stays unchanged
// — it records the size and emits an EventAttachment with NO Content bytes.
func TestAttachMetadataOnlyEmitsNoContent(t *testing.T) {
	res := &Result{}
	cap := &captureEmitter{}
	seed := NewRunSeed(2, true).ForTest("attach/meta")
	tt := newT(context.Background(), nil, seed, nil, nil, res, cap)

	tt.Attach("big.bin", "artifact", 4096)

	if len(res.Attachments) != 1 || res.Attachments[0].Size != 4096 {
		t.Fatalf("recorded attachments = %+v, want one size-4096", res.Attachments)
	}
	if len(cap.events) != 1 || cap.events[0].Content != nil {
		t.Errorf("metadata-only attach emitted Content %q, want nil", cap.events[0].Content)
	}
}

// TestResourceSuffixStableWithinTest: the suffix must be stable across calls in
// one execution, else different resources within a test would get different
// tags and teardown/reap would miss some.
func TestResourceSuffixStableWithinTest(t *testing.T) {
	seed := NewRunSeed(0xabcd, true).ForTest("stable/test")
	tt := newT(context.Background(), nil, seed, nil, nil, &Result{}, nil)
	if tt.ResourceSuffix() != tt.ResourceSuffix() {
		t.Fatalf("suffix not stable: %q then %q", tt.ResourceSuffix(), tt.ResourceSuffix())
	}
}

// TestResourceSuffixIdentifierSafe: the token is all-hex so it is safe inside an
// unquoted SQL identifier and a DNS label.
func TestResourceSuffixIdentifierSafe(t *testing.T) {
	seed := NewRunSeed(-1, true).ForTest("safe/test")
	s := newT(context.Background(), nil, seed, nil, nil, &Result{}, nil).ResourceSuffix()
	if s == "" {
		t.Fatal("empty suffix")
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("suffix %q has non-hex rune %q (not SQL/DNS safe)", s, r)
		}
	}
}
