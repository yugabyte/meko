package ampbackend

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"meko/ycp/internal/ampclient"
	"meko/ycp/reconcile"
)

// The address alone said "ready" for every endpoint ever deployed, crash loop
// or not (meko#1746). A health verdict decides where there is one, and an old
// one keeps its state but says how old it is.
func TestEndpointState(t *testing.T) {
	why := "container documentdb-gateway (image gw:1) cannot start: CrashLoopBackOff"
	now := time.Date(2026, 10, 7, 5, 30, 0, 0, time.UTC)
	fresh, stale := now.Add(-time.Minute), now.Add(-healthStaleAfter)
	down := &ampclient.EndpointHealth{Status: ampclient.NotReady, Reason: &why}
	up := &ampclient.EndpointHealth{Status: ampclient.Ready}
	tests := []struct {
		name       string
		address    string
		health     *ampclient.EndpointHealth
		readAt     time.Time
		wantReady  bool
		wantState  string
		wantDetail string
	}{
		{name: "not deployed yet", wantState: "provisioning"},
		{name: "no verdict falls back to the address", address: "h:5432", wantReady: true, wantState: "ready"},
		{name: "healthy", address: "h:5432", health: up, readAt: fresh, wantReady: true, wantState: "ready"},
		{name: "down keeps its address and is still not ready", address: "h:5432", health: down, readAt: fresh,
			wantState: "not_ready", wantDetail: why},
		{name: "stale down stays down and says how old", address: "h:5432", health: down, readAt: stale,
			wantState: "not_ready", wantDetail: "health not read since 2026-10-07T05:20:00Z; last: " + why},
		{name: "stale up stays up and says how old", address: "h:5432", health: up, readAt: stale,
			wantReady: true, wantState: "ready", wantDetail: "health not read since 2026-10-07T05:20:00Z; last reading ready"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, state, detail := endpointState(tt.address, tt.health, tt.readAt, now)
			if ready != tt.wantReady || state != tt.wantState || detail != tt.wantDetail {
				t.Errorf("endpointState = (%v, %q, %q), want (%v, %q, %q)",
					ready, state, detail, tt.wantReady, tt.wantState, tt.wantDetail)
			}
		})
	}
}

// An endpoint with no address is either still being created or already failed;
// only the task that created it can say which (CLOUDGA-37682). Every other case
// must leave the row to endpointState, or a healthy endpoint reads as failed.
func TestEndpointCreateFailure(t *testing.T) {
	id := uuid.MustParse("77777777-0000-0000-0000-000000000001")
	cause := "deploy endpoint: 0/6 nodes are available: 3 Insufficient ephemeral-storage"
	empty := ""
	task := func(kind string, status ampclient.TaskStatus, err *string) *ampclient.EndpointCreateTask {
		return &ampclient.EndpointCreateTask{Id: id, Kind: kind, Status: status, Error: err}
	}
	tests := []struct {
		name    string
		address string
		ct      *ampclient.EndpointCreateTask
		want    string // "" = no failure
	}{
		{name: "standalone create failed", ct: task("create_endpoint", ampclient.TaskStatusFailed, &cause),
			want: "create_endpoint task " + id.String() + " failed: " + cause},
		{name: "writer of a create_cluster failed", ct: task("create_cluster", ampclient.TaskStatusFailed, &cause),
			want: "create_cluster task " + id.String() + " failed: " + cause},
		{name: "writer of a branch failed", ct: task("branch", ampclient.TaskStatusFailed, &cause),
			want: "branch task " + id.String() + " failed: " + cause},
		{name: "failed with no error", ct: task("create_endpoint", ampclient.TaskStatusFailed, nil),
			want: "create_endpoint task " + id.String() + " failed: no error reported"},
		{name: "failed with an empty error", ct: task("create_endpoint", ampclient.TaskStatusFailed, &empty),
			want: "create_endpoint task " + id.String() + " failed: no error reported"},
		{name: "failed with no kind", ct: task("", ampclient.TaskStatusFailed, &cause),
			want: "create task " + id.String() + " failed: " + cause},
		{name: "create pending", ct: task("create_endpoint", ampclient.TaskStatusPending, nil)},
		{name: "create running", ct: task("create_endpoint", ampclient.TaskStatusRunning, nil)},
		{name: "create succeeded but no address yet", ct: task("create_endpoint", ampclient.TaskStatusSucceeded, nil)},
		{name: "a status this agent does not know", ct: task("create_endpoint", "cancelled", &cause)},
		{name: "controller that predates create_task"},
		// The deploy landed; a later step of the same task failing does not make the
		// endpoint a failed create. Its health decides.
		{name: "has an address", address: "h:5433", ct: task("create_cluster", ampclient.TaskStatusFailed, &cause)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := endpointCreateFailure(tt.address, tt.ct)
			switch {
			case tt.want == "" && got != nil:
				t.Errorf("failure = %v, want none", got)
			case tt.want != "" && (got == nil || got.Error() != tt.want):
				t.Errorf("failure = %v, want %q", got, tt.want)
			}
		})
	}
}

// A tick that comes back without health — the controller's pod read failed, or a
// status this agent does not know — repeats the endpoint's last verdict and when
// it was read, rather than flipping a down endpoint to ready.
func TestEndpointHealthHoldsLastVerdict(t *testing.T) {
	clock := time.Date(2026, 10, 7, 5, 0, 0, 0, time.UTC)
	b := &Backend{now: func() time.Time { return clock }}
	key := reconcile.ResourceKey{Kind: KindEndpoint, Name: "p/c/main/rw"}
	why := "container endpoint exited with code 1"
	down := &ampclient.EndpointHealth{Status: ampclient.NotReady, Reason: &why}

	if got, _ := b.endpointHealth(key, nil); got != nil {
		t.Fatalf("never reported: got %+v, want nil", got)
	}
	readAt := clock
	if got, at := b.endpointHealth(key, down); got == nil || got.Status != ampclient.NotReady || !at.Equal(readAt) {
		t.Fatalf("reported not_ready: got %+v at %v", got, at)
	}
	clock = clock.Add(healthStaleAfter + time.Minute)
	for _, h := range []*ampclient.EndpointHealth{nil, {Status: "degraded"}} {
		got, at := b.endpointHealth(key, h)
		if got == nil || got.Status != ampclient.NotReady || *got.Reason != why || !at.Equal(readAt) {
			t.Errorf("health %+v after not_ready: got %+v read at %v, want the held not_ready read at %v", h, got, at, readAt)
		}
		// End to end: still down, and flagged as an old reading.
		if ready, state, detail := endpointState("h:5432", got, at, clock); ready || state != "not_ready" ||
			!strings.HasPrefix(detail, "health not read since ") {
			t.Errorf("held stale not_ready = (%v, %q, %q), want not_ready flagged stale", ready, state, detail)
		}
	}
	if got, at := b.endpointHealth(key, &ampclient.EndpointHealth{Status: ampclient.Ready}); got.Status != ampclient.Ready || !at.Equal(clock) {
		t.Errorf("recovered: got %+v at %v, want ready read now", got, at)
	}
}
