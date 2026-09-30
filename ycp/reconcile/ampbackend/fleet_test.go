package ampbackend

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"meko/ycp/internal/api"
	"meko/ycp/reconcile"
)

const (
	fleetOld = "0.260901.0-abc1234"
	fleetNew = controllerTargetVersion
)

func fleetRow(t *testing.T, b *Backend) (reconcile.Actual, bool) {
	t.Helper()
	rows, err := b.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, a := range rows {
		if a.Key.Kind == KindFleetVersion {
			return a, true
		}
	}
	return reconcile.Actual{}, false
}

// Until a fleet-version is declared to this process the row does not exist, so a
// declaration is planned as a Create — which converges exactly like an Update.
func TestFleetRowAppearsOnlyOnceDeclared(t *testing.T) {
	g := &genServer{branchGen: gen64(1), endpointGen: gen64(1), fleetVersion: fleetOld, fleetPin: fleetNew}
	b := g.start(t)
	if a, ok := fleetRow(t, b); ok {
		t.Fatalf("fleet row listed before any declaration: %+v", a)
	}
	mustIssue(t, b.Create(t.Context(), fleetRes(1)))
	if g.fleetRolls != 1 {
		t.Errorf("Create rolled %d times, want 1", g.fleetRolls)
	}
	a, ok := fleetRow(t, b)
	if !ok {
		t.Fatal("no fleet row after a declaration")
	}
	if !a.Preexisting || a.ObservedGen != reconcile.GenerationNever {
		t.Errorf("row = %+v, want Preexisting at GenerationNever — the fleet is never created or deleted", a)
	}
}

func TestFleetUpdate(t *testing.T) {
	for name, tc := range map[string]struct {
		g         genServer
		wantRolls int
		want      string // "nil", "pending", "error"
	}{
		"behind a controller at the target rolls":   {g: genServer{fleetVersion: fleetOld, fleetPin: fleetNew}, wantRolls: 1, want: "pending"},
		"converged does nothing":                    {g: genServer{fleetVersion: fleetNew, fleetPin: fleetNew}, want: "nil"},
		"a roll in flight waits":                    {g: genServer{fleetVersion: fleetOld, fleetPin: fleetNew, fleetRoll: "running"}, want: "pending"},
		"a controller not yet at the target waits":  {g: genServer{fleetVersion: fleetOld, fleetPin: fleetOld}, want: "pending"},
		"a roll that raced this one is pending":     {g: genServer{fleetVersion: fleetOld, fleetPin: fleetNew, mutateStatus: http.StatusConflict}, wantRolls: 1, want: "pending"},
		"a controller without the route is retried": {g: genServer{fleetMissing: true}, want: "error"},
	} {
		t.Run(name, func(t *testing.T) {
			g := tc.g
			g.branchGen, g.endpointGen = gen64(1), gen64(1)
			b := g.start(t)
			err := b.Update(t.Context(), fleetRes(2))

			var pending *reconcile.PendingError
			var perm *reconcile.PermanentError
			switch tc.want {
			case "nil":
				if err != nil {
					t.Errorf("Update = %v, want nil", err)
				}
			case "pending":
				if !errors.As(err, &pending) {
					t.Errorf("Update = %v, want Pending", err)
				}
			case "error":
				if err == nil || errors.As(err, &pending) || errors.As(err, &perm) {
					t.Errorf("Update = %v, want a retryable error", err)
				}
			}
			if g.fleetRolls != tc.wantRolls {
				t.Errorf("rolls = %d, want %d", g.fleetRolls, tc.wantRolls)
			}
		})
	}
}

// A roll that fails is reported with the controller's error on the next tick, and
// the one after re-issues it — which re-runs what the failed roll left behind.
func TestFleetUpdateReportsAFailedRoll(t *testing.T) {
	g := &genServer{branchGen: gen64(1), endpointGen: gen64(1), fleetVersion: fleetOld, fleetPin: fleetNew}
	b := g.start(t)
	mustIssue(t, b.Update(t.Context(), fleetRes(2)))

	g.taskStatus, g.taskError = "failed", "endpoint e1: helm --wait timed out"
	err := b.Update(t.Context(), fleetRes(2))
	if err == nil || !strings.Contains(err.Error(), "helm --wait timed out") {
		t.Fatalf("Update after a failed roll = %v, want the task's error", err)
	}
	mustIssue(t, b.Update(t.Context(), fleetRes(2)))
	if g.fleetRolls != 2 {
		t.Errorf("rolls = %d, want the failed one re-issued", g.fleetRolls)
	}
}

func TestFleetUpdateRefusesABadDeclaration(t *testing.T) {
	g := &genServer{branchGen: gen64(1), endpointGen: gen64(1), fleetVersion: fleetOld, fleetPin: fleetNew}
	b := g.start(t)
	for name, res := range map[string]api.DesiredResource{
		"not named fleet": {Kind: KindFleetVersion, Name: "other", Spec: json.RawMessage(`{"version":"v2"}`)},
		"no version":      {Kind: KindFleetVersion, Name: fleetName, Spec: json.RawMessage(`{}`)},
	} {
		var perm *reconcile.PermanentError
		if err := b.Update(t.Context(), res); !errors.As(err, &perm) {
			t.Errorf("%s: Update = %v, want a PermanentError", name, err)
		}
	}
	if g.fleetRolls != 0 {
		t.Errorf("a refused declaration rolled %d times", g.fleetRolls)
	}
}

func TestFleetRowState(t *testing.T) {
	for name, tc := range map[string]struct {
		g            genServer
		wantState    string
		wantReady    bool
		wantProgress string
	}{
		"at the declared version": {g: genServer{fleetVersion: fleetNew, fleetPin: fleetNew}, wantState: fleetStateReady, wantReady: true},
		"behind":                  {g: genServer{fleetVersion: fleetOld, fleetPin: fleetNew}, wantState: fleetStateBehind},
		"mixed":                   {g: genServer{fleetVersion: "mixed", fleetPin: fleetNew}, wantState: fleetStateBehind},
		"rolling":                 {g: genServer{fleetVersion: fleetOld, fleetPin: fleetNew, fleetRoll: "running", fleetRollDone: 1, fleetRollTotal: 3}, wantState: fleetStateRolling, wantProgress: "1/3"},
		"queued":                  {g: genServer{fleetVersion: fleetOld, fleetPin: fleetNew, fleetRoll: "pending"}, wantState: fleetStateQueued},
		"controller without it":   {g: genServer{fleetMissing: true}, wantState: fleetStateUpgrading},
	} {
		t.Run(name, func(t *testing.T) {
			g := tc.g
			g.branchGen, g.endpointGen = gen64(1), gen64(1)
			b := g.start(t)
			_ = b.Update(t.Context(), fleetRes(2))

			a, ok := fleetRow(t, b)
			if !ok {
				t.Fatal("no fleet row after a declaration")
			}
			if a.State != tc.wantState || a.Ready != tc.wantReady || a.Progress != tc.wantProgress {
				t.Errorf("row = state %q ready %v progress %q, want %q %v %q",
					a.State, a.Ready, a.Progress, tc.wantState, tc.wantReady, tc.wantProgress)
			}
			if a.Attrs["target_version"] != fleetNew {
				t.Errorf("attrs = %v, want target_version %s", a.Attrs, fleetNew)
			}
		})
	}
}

func TestFleetVersionParentIsTheController(t *testing.T) {
	b := (&genServer{branchGen: gen64(1), endpointGen: gen64(1)}).start(t)
	parent, ok := b.Parent(fleetRes(1))
	if !ok || parent != (reconcile.ResourceKey{Kind: KindController, Name: controllerName}) {
		t.Errorf("Parent = %v, %v, want the controller", parent, ok)
	}
}

// A subtask read that fails repeats the last progress rather than reporting none:
// a change is what restarts the stuck timers, so flipping between "k/n" and ""
// would read a wedged roll as one that keeps moving.
func TestFleetRollProgressSurvivesAFailedRead(t *testing.T) {
	g := &genServer{branchGen: gen64(1), endpointGen: gen64(1), fleetVersion: fleetOld, fleetPin: fleetNew,
		fleetRoll: "running", fleetRollDone: 1, fleetRollTotal: 3}
	b := g.start(t)
	_ = b.Update(t.Context(), fleetRes(2))
	if a, _ := fleetRow(t, b); a.Progress != "1/3" {
		t.Fatalf("progress = %q, want 1/3", a.Progress)
	}
	g.fleetSubtasksFail = true
	if a, _ := fleetRow(t, b); a.Progress != "1/3" {
		t.Errorf("progress after a failed read = %q, want the last value 1/3", a.Progress)
	}
}
