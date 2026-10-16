package ampbackend

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"meko/ycp/reconcile"

	"meko/ycp/internal/api"
)

const (
	schedTolerations = `[{"effect":"NoSchedule","key":"dedicated","operator":"Equal","value":"amp"}]`
	schedSpread      = `[{"labelSelector":{"matchLabels":{"amp.yugabyte.com/cluster-id":"c1"}},"maxSkew":1,"topologyKey":"topology.kubernetes.io/zone","whenUnsatisfiable":"ScheduleAnyway"}]`
)

func schedRes(gen int64, spec string) api.DesiredResource {
	return api.DesiredResource{Kind: KindEndpoint, Name: genEndpointPath, Generation: gen, Spec: json.RawMessage(spec)}
}

// A changed list goes out as its own task carrying the generation; the list the
// spec does not declare is not sent, so the controller keeps it.
func TestUpdateEndpointSchedulingSendsOnlyTheChangedList(t *testing.T) {
	g := &genServer{endpointGen: gen64(1), endpointSettings: `,"topology_spread_constraints":` + schedSpread}
	b := g.start(t)

	mustIssue(t, b.Update(t.Context(), schedRes(4,
		`{"tolerations":`+schedTolerations+`,"topology_spread_constraints":`+schedSpread+`}`)))

	if len(g.schedUpdates) != 1 {
		t.Fatalf("scheduling calls = %d, want 1", len(g.schedUpdates))
	}
	req := g.schedUpdates[0]
	if req.Tolerations == nil || mustJSON(t, *req.Tolerations) != schedTolerations {
		t.Errorf("sent tolerations = %v, want %s", req.Tolerations, schedTolerations)
	}
	if req.TopologySpreadConstraints != nil {
		t.Errorf("sent spread = %v, want none: the row already carries it", *req.TopologySpreadConstraints)
	}
	if req.YcpGeneration == nil || *req.YcpGeneration != 4 {
		t.Errorf("ycp_generation = %v, want 4", req.YcpGeneration)
	}
}

// Declared lists the row already reports — `[]` included, which the controller
// reports by omitting the field — issue no task; the generation is stamped by
// the empty settings PATCH instead.
func TestUpdateEndpointSchedulingConvergedSendsNothing(t *testing.T) {
	g := &genServer{endpointGen: gen64(1), endpointSettings: `,"tolerations":` + schedTolerations}
	b := g.start(t)

	if err := b.Update(t.Context(), schedRes(4,
		`{"tolerations":`+schedTolerations+`,"topology_spread_constraints":[]}`)); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(g.schedUpdates) != 0 {
		t.Errorf("scheduling calls = %d, want none", len(g.schedUpdates))
	}
	if len(g.epPatches) != 1 || g.epPatches[0].YcpGeneration == nil || *g.epPatches[0].YcpGeneration != 4 {
		t.Errorf("settings patches = %+v, want one stamping generation 4", g.epPatches)
	}
}

// Clearing is a declared `[]` against a row that carries a list.
func TestUpdateEndpointSchedulingClears(t *testing.T) {
	g := &genServer{endpointGen: gen64(1), endpointSettings: `,"tolerations":` + schedTolerations}
	b := g.start(t)

	mustIssue(t, b.Update(t.Context(), schedRes(4, `{"tolerations":[]}`)))
	if len(g.schedUpdates) != 1 || g.schedUpdates[0].Tolerations == nil || len(*g.schedUpdates[0].Tolerations) != 0 {
		t.Fatalf("scheduling calls = %+v, want one sending tolerations []", g.schedUpdates)
	}
}

// With a resize also declared, the scheduling task goes first without the
// generation, and the resize carries it on the next tick.
func TestUpdateEndpointSchedulingGoesBeforeTheResize(t *testing.T) {
	g := &genServer{endpointGen: gen64(1)}
	b := g.start(t)
	spec := `{"resources":{"cpu":"4","memory":"8Gi"},"tolerations":` + schedTolerations + `}`

	mustIssue(t, b.Update(t.Context(), schedRes(4, spec)))
	if len(g.schedUpdates) != 1 || g.schedUpdates[0].YcpGeneration != nil {
		t.Fatalf("scheduling calls = %+v, want one carrying no generation", g.schedUpdates)
	}
	if len(g.resizes) != 0 {
		t.Fatalf("resize calls = %d, want none until the scheduling task is done", len(g.resizes))
	}

	g.taskStatus = "succeeded"
	g.endpointSettings = `,"tolerations":` + schedTolerations
	if _, err := b.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}
	mustIssue(t, b.Update(t.Context(), schedRes(4, spec)))
	if len(g.schedUpdates) != 1 {
		t.Errorf("scheduling calls = %d, want no second one: the row now matches", len(g.schedUpdates))
	}
	if len(g.resizes) != 1 || g.resizes[0].YcpGeneration == nil || *g.resizes[0].YcpGeneration != 4 {
		t.Errorf("resizes = %+v, want one carrying generation 4", g.resizes)
	}
}

// A drifted update is never re-issued, so scheduling and the resize both go out
// on this tick — scheduling first, since its re-render recreates the pod.
func TestUpdateEndpointSchedulingDriftGoesBeforeTheResize(t *testing.T) {
	g := &genServer{endpointGen: gen64(1)}
	b := g.start(t)

	err := b.Update(t.Context(), schedRes(4,
		`{"resources":{"cpu":"4","memory":"8Gi"},"role":"read_only","tolerations":`+schedTolerations+`}`))
	var perm *reconcile.PermanentError
	if !errors.As(err, &perm) {
		t.Fatalf("Update = %v, want permanent drift", err)
	}
	if want := []string{"scheduling", "resize"}; !slices.Equal(g.mutations, want) {
		t.Errorf("calls = %v, want %v", g.mutations, want)
	}
}

// Both lists are reported on every tick, "[]" when the row has none: an absent
// attr means an agent that predates them.
func TestListReportsScheduling(t *testing.T) {
	for name, tc := range map[string]struct {
		row, wantTols, wantSpread string
	}{
		"set":   {`,"tolerations":` + schedTolerations + `,"topology_spread_constraints":` + schedSpread, schedTolerations, schedSpread},
		"unset": {"", "[]", "[]"},
	} {
		t.Run(name, func(t *testing.T) {
			b := (&genServer{endpointSettings: tc.row}).start(t)
			actuals, err := b.List(t.Context())
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			for _, a := range actuals {
				if a.Key.Kind != KindEndpoint {
					continue
				}
				if got := a.Attrs["tolerations"]; got != tc.wantTols {
					t.Errorf("tolerations = %q, want %q", got, tc.wantTols)
				}
				if got := a.Attrs["topology_spread_constraints"]; got != tc.wantSpread {
					t.Errorf("topology_spread_constraints = %q, want %q", got, tc.wantSpread)
				}
			}
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
