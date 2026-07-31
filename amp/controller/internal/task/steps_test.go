package task

import (
	"encoding/json"
	"testing"
)

// These tests exercise each step (one per step_<kind>.go) against the in-memory
// ports, focusing on the property the framework rests on: idempotency. Every step
// is run, then run a second time, and must converge — no duplicate effect, no
// error — so the worker can safely replay the in-flight step after a crash (§8.1).
// They also cover error propagation and the fork-vs-default-branch deploy spec.
// The per-step tests live alongside their step in step_<kind>_test.go; this file
// holds the shared test helpers. (Bad-args rejection now lives at the Registry
// reconstruction seam — see registry_test.go's TestRegistryBuildRejectsBadArgs.)

// decodeResult unmarshals a StepResult's output into a generic map for assertions.
func decodeResult(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return m
}
