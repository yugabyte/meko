package foundation

import (
	"context"
	"strings"
	"testing"
)

// TestResourceStatusMirrorsEachKindsOwnShape.
//
// Three kinds in the bundle report readiness three different ways, and none of
// it is guessable from the CRD: the providers have `state`, the certificate has
// a `success` bool, and the universe has `universeState` plus `actions`. Status
// has to read all three, because a reference that never becomes ready is only
// visible if its own wording is carried through.
func TestResourceStatusMirrorsEachKindsOwnShape(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    string
		body    string
		want    ResourceState
		wantRaw string
		inMsg   string
	}{{
		name: "provider ready",
		kind: "YBProvider",
		body: `{"status":{"state":"READY","resourceUUID":"p1"}}`,
		want: ResourceReady, wantRaw: "READY",
	}, {
		name: "provider in error",
		kind: "YBProvider",
		body: `{"status":{"state":"ERROR","message":"bad credentials"}}`,
		want: ResourceFailed, wantRaw: "ERROR", inMsg: "bad credentials",
	}, {
		name: "provider mid-update is not a failure",
		kind: "YBProvider",
		body: `{"status":{"state":"UPDATING"}}`,
		want: ResourcePending, wantRaw: "UPDATING",
	}, {
		// A registration a universe holds is as usable as Ready; treating it as
		// anything else would report a working foundation as not ready.
		name: "kms in use counts as ready",
		kind: "KMSConfig",
		body: `{"status":{"state":"InUse","resourceUUID":"k1"}}`,
		want: ResourceReady, wantRaw: "InUse",
	}, {
		name: "certificate rejected",
		kind: "YBCertificate",
		body: `{"status":{"success":false,"message":"Certificate must end with a newline"}}`,
		want: ResourceFailed, inMsg: "must end with a newline",
	}, {
		name: "certificate accepted",
		kind: "YBCertificate",
		body: `{"status":{"success":true,"resourceUUID":"c1"}}`,
		want: ResourceReady,
	}, {
		name: "universe settled",
		kind: "YBUniverse",
		body: `{"status":{"universeState":"Ready","actions":[]}}`,
		want: ResourceReady, wantRaw: "Ready",
	}, {
		// Ready with work still queued is NOT settled: the state can be left
		// over from the previous generation, which is why Settled looks at
		// actions too.
		name: "universe ready with an action queued",
		kind: "YBUniverse",
		body: `{"status":{"universeState":"Ready","actions":[{"action_type":"EditUniverse","status":"queued","taskUUID":"t1"}]}}`,
		want: ResourcePending, wantRaw: "Ready",
	}, {
		// Reported, not adjudicated — YBA requeues CREATE forever, so this is
		// a state that heals.
		name: "universe in error is pending, not failed",
		kind: "YBUniverse",
		body: `{"status":{"universeState":"Error Creating","actions":[{"action_type":"CreateKubernetesUniverse","status":"queued"}]}}`,
		want: ResourcePending, wantRaw: "Error Creating",
	}, {
		// A kind with no readiness field at all still reports something,
		// rather than needing a case added here first.
		name: "an unknown shape says so",
		kind: "Backup",
		body: `{"status":{"taskUUID":"t9"}}`,
		want: ResourcePending, inMsg: "says nothing about readiness",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &Recorder{Responses: []Response{
				{Match: "get " + strings.ToLower(tc.kind), Result: Result{Stdout: tc.body}},
			}}
			d, _ := testDeployer(t, rec)
			got := d.resourceStatus(context.Background(), tc.kind, "x")
			if got.State != tc.want {
				t.Errorf("State = %q, want %q (message %q)", got.State, tc.want, got.Message)
			}
			if tc.wantRaw != "" && got.Raw != tc.wantRaw {
				t.Errorf("Raw = %q, want %q — the CR's own wording is what a reader needs",
					got.Raw, tc.wantRaw)
			}
			if tc.inMsg != "" && !strings.Contains(got.Message, tc.inMsg) {
				t.Errorf("Message = %q, want it to contain %q", got.Message, tc.inMsg)
			}
		})
	}
}

// A CR the spec declares but the cluster does not have reads Absent rather than
// erroring: between applying a spec and YBA creating anything, that is the
// normal state.
func TestResourceStatusReportsAnAbsentCR(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get ybuniverse", Result: Result{ExitCode: 1, Stderr: "NotFound"}},
	}}
	d, _ := testDeployer(t, rec)
	got := d.resourceStatus(context.Background(), "YBUniverse", "foundation")
	if got.State != ResourceAbsent {
		t.Errorf("State = %q, want %q", got.State, ResourceAbsent)
	}
}

// Phase is the one-word answer, and Ready means REACHABLE: a foundation whose
// CRs are all Ready but which has produced no address yet cannot be used, so
// reporting Ready would send a consumer at nothing.
func TestPhaseNeedsAnAddressBeforeReady(t *testing.T) {
	ready := []ResourceStatus{{Kind: "YBUniverse", State: ResourceReady}}
	yba := YBAStatus{Installed: true}

	if got := phaseOf(&Status{YBA: yba, Resources: ready}); got != PhaseConverging {
		t.Errorf("phase with no address = %q, want %q", got, PhaseConverging)
	}
	withAddr := &Status{YBA: yba, Resources: ready, Address: &Info{YSQLHostPorts: "svc:5433"}}
	if got := phaseOf(withAddr); got != PhaseReady {
		t.Errorf("phase with everything ready = %q, want %q", got, PhaseReady)
	}
	// One failure outranks everything, including a missing YBA.
	failed := &Status{Resources: []ResourceStatus{{Kind: "YBCertificate", State: ResourceFailed}}}
	if got := phaseOf(failed); got != PhaseFailed {
		t.Errorf("phase with a failed resource = %q, want %q", got, PhaseFailed)
	}
	// YBA not up yet is Converging even with nothing declared.
	if got := phaseOf(&Status{}); got != PhaseConverging {
		t.Errorf("phase with no YBA = %q, want %q", got, PhaseConverging)
	}
}

// PublishConn writes the PARTS, not a DSN: the cert paths in a finished DSN are
// facts about the consumer's filesystem, so one baked value cannot be right for
// every consumer.
func TestPublishConnWritesThePartsNotADSN(t *testing.T) {
	rec := &Recorder{Responses: []Response{{Match: "apply", Result: Result{}}}}
	d, _ := testDeployer(t, rec)
	info := &Info{
		YSQLHostPorts:     "foundation-123-yb-tserver-conn.amp.svc.cluster.local:5433",
		UniverseNamespace: "amp",
		DBURL:             "postgres://yugabyte@host/yb_storage?sslmode=verify-full",
	}
	if err := d.PublishConn(context.Background(), "amp", info); err != nil {
		t.Fatalf("PublishConn: %v", err)
	}
	var body string
	for _, c := range rec.Calls {
		if c.Stdin != "" {
			body = c.Stdin
		}
	}
	for _, want := range []string{"YB_HOST_PORTS", "YB_SSLMODE", "YB_UNIVERSE_NAMESPACE", ConnConfigMap} {
		if !strings.Contains(body, want) {
			t.Errorf("the ConfigMap is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "DB_URL") || strings.Contains(body, "postgres://") {
		t.Errorf("the ConfigMap carries a baked DSN, whose cert paths belong to the consumer:\n%s", body)
	}
	if strings.Contains(body, `"kind":"Secret"`) {
		t.Error("published a Secret; the DSN has no password and the Service name is not secret")
	}
}

// Publishing with no address is refused rather than writing an empty ConfigMap
// a consumer would mount and read as a valid, blank answer.
func TestPublishConnRefusesWithoutAnAddress(t *testing.T) {
	d, _ := testDeployer(t, &Recorder{})
	if err := d.PublishConn(context.Background(), "amp", nil); err == nil {
		t.Fatal("published an empty address")
	}
}

// The kind-keyed projection the printer columns read. Several objects of one
// kind collapse to the state furthest from ready, because a column reading
// "Ready" while one of two providers is failing is worse than no column.
func TestResourceStatesKeepTheWorstPerKind(t *testing.T) {
	got := resourceStates([]ResourceStatus{
		{Kind: "YBProvider", Name: "a", State: ResourceReady},
		{Kind: "YBProvider", Name: "b", State: ResourceFailed},
		{Kind: "YBUniverse", Name: "u", State: ResourcePending},
		{Kind: "YBCertificate", Name: "c", State: ResourceReady},
	})
	for kind, want := range map[string]ResourceState{
		"YBProvider":    ResourceFailed,
		"YBUniverse":    ResourcePending,
		"YBCertificate": ResourceReady,
	} {
		if got[kind] != want {
			t.Errorf("resourceStates[%q] = %q, want %q", kind, got[kind], want)
		}
	}
	// Order must not decide it: the same two providers the other way round.
	flipped := resourceStates([]ResourceStatus{
		{Kind: "YBProvider", Name: "b", State: ResourceFailed},
		{Kind: "YBProvider", Name: "a", State: ResourceReady},
	})
	if flipped["YBProvider"] != ResourceFailed {
		t.Errorf("list order changed the answer: got %q", flipped["YBProvider"])
	}
	// Absent outranks Pending: a CR that does not exist is further from ready
	// than one that exists and is working.
	mixed := resourceStates([]ResourceStatus{
		{Kind: "YBProvider", State: ResourcePending},
		{Kind: "YBProvider", State: ResourceAbsent},
	})
	if mixed["YBProvider"] != ResourceAbsent {
		t.Errorf("Absent did not outrank Pending: got %q", mixed["YBProvider"])
	}
}
