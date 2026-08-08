package task

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// These tests validate the task Service (tasks.go): each CreateX method records
// the right ordered step kinds, generates and threads ids consistently across the
// steps (so a downstream step operates on the same cluster/branch/endpoint an
// upstream step created), and gets the fork vs. default-branch (ancestor) wiring
// right. They run the Service against an in-memory MemStore and read the
// persisted steps back to assert on them.

func stepKinds(steps []StepRecord) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Kind
	}
	return out
}

func stepByKind(t *testing.T, steps []StepRecord, kind string) StepRecord {
	t.Helper()
	for _, s := range steps {
		if s.Kind == kind {
			return s
		}
	}
	t.Fatalf("no %q step in task (got %v)", kind, stepKinds(steps))
	return StepRecord{}
}

func decodeArgs(t *testing.T, st StepRecord, dst any) {
	t.Helper()
	if err := json.Unmarshal(st.Args, dst); err != nil {
		t.Fatalf("decode %q args: %v", st.Kind, err)
	}
}

func equalKinds(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestServiceCreateClusterNoWriter(t *testing.T) {
	store := NewMemStore()
	svc := NewService(store)
	poolID := uuid.New()
	tk, err := svc.CreateCluster(t.Context(), CreateClusterInput{PoolID: poolID, Name: "shop", PgVersion: 18})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if tk.Kind != KindCreateCluster {
		t.Errorf("kind = %q, want %q", tk.Kind, KindCreateCluster)
	}
	steps, _ := store.Steps(t.Context(), tk.ID)
	want := []string{StepCreateCluster, StepRecordBranchRecipe}
	if !equalKinds(stepKinds(steps), want) {
		t.Fatalf("steps = %v, want %v", stepKinds(steps), want)
	}

	var cp CreateClusterArgs
	decodeArgs(t, stepByKind(t, steps, StepCreateCluster), &cp)
	if cp.PoolID != poolID || cp.Name != "shop" || cp.PgVersion != 18 {
		t.Errorf("create-cluster args = %+v, want pool=%s name=shop pgv=18", cp, poolID)
	}
	if cp.ClusterID == uuid.Nil || cp.BranchID == uuid.Nil {
		t.Error("cluster/branch ids were not generated")
	}

	var rec RecordBranchRecipeArgs
	decodeArgs(t, stepByKind(t, steps, StepRecordBranchRecipe), &rec)
	// The recipe step must target the same cluster+branch the create step makes.
	if rec.ClusterID != cp.ClusterID || rec.BranchID != cp.BranchID {
		t.Errorf("recipe ids (%s,%s) != create-cluster ids (%s,%s)", rec.ClusterID, rec.BranchID, cp.ClusterID, cp.BranchID)
	}
	if rec.AncestorBranchID != nil {
		t.Error("default-branch recipe has an ancestor, want nil")
	}
	if rec.StartLSN != "0/0" {
		t.Errorf("default-branch recipe start_lsn = %q, want 0/0", rec.StartLSN)
	}

	// The task targets the cluster it creates (stamped for the task API).
	if tk.ClusterID == nil || *tk.ClusterID != cp.ClusterID {
		t.Errorf("task ClusterID = %v, want %s", tk.ClusterID, cp.ClusterID)
	}
	// Params round-trip to the originating request.
	var got CreateClusterInput
	if err := json.Unmarshal(tk.Params, &got); err != nil || got.Name != "shop" || got.PoolID != poolID {
		t.Errorf("params round-trip = %+v (err %v), want name=shop pool=%s", got, err, poolID)
	}
}

func TestServiceCreateClusterWithWriter(t *testing.T) {
	store := NewMemStore()
	svc := NewService(store)
	tk, err := svc.CreateCluster(t.Context(), CreateClusterInput{PoolID: uuid.New(), Name: "shop", PgVersion: 18, WithWriter: true, CPU: "4", Memory: "8Gi"})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	steps, _ := store.Steps(t.Context(), tk.ID)
	want := []string{StepCreateCluster, StepRecordBranchRecipe, StepCreateEndpoint}
	if !equalKinds(stepKinds(steps), want) {
		t.Fatalf("steps = %v, want %v", stepKinds(steps), want)
	}

	var cp CreateClusterArgs
	decodeArgs(t, stepByKind(t, steps, StepCreateCluster), &cp)
	var ep CreateEndpointArgs
	decodeArgs(t, stepByKind(t, steps, StepCreateEndpoint), &ep)
	if ep.ClusterID != cp.ClusterID || ep.BranchID != cp.BranchID {
		t.Errorf("endpoint targets (%s,%s), want the created branch (%s,%s)", ep.ClusterID, ep.BranchID, cp.ClusterID, cp.BranchID)
	}
	if ep.Role != "read_write" {
		t.Errorf("first endpoint role = %q, want read_write", ep.Role)
	}
	if ep.EndpointID == uuid.Nil {
		t.Error("endpoint id was not generated")
	}
	// The requested compute size must be threaded into the writer step (rather
	// than dropped, which would silently fall back to the deployer default).
	if ep.CPU != "4" || ep.Memory != "8Gi" {
		t.Errorf("writer size = %q/%q, want 4/8Gi", ep.CPU, ep.Memory)
	}
}

func TestServiceBranch(t *testing.T) {
	store := NewMemStore()
	svc := NewService(store)
	poolID, clusterID, parentID := uuid.New(), uuid.New(), uuid.New()
	tk, err := svc.Branch(t.Context(), BranchInput{
		PoolID: poolID, ClusterID: clusterID, ParentBranchID: parentID,
		Name: "feature", ForkLSN: "0/16ABCD0", PgVersion: 18, WithWriter: true,
	})
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	if tk.Kind != KindBranch {
		t.Errorf("kind = %q, want %q", tk.Kind, KindBranch)
	}
	steps, _ := store.Steps(t.Context(), tk.ID)
	want := []string{StepRecordBranchRecipe, StepCreateEndpoint}
	if !equalKinds(stepKinds(steps), want) {
		t.Fatalf("steps = %v, want %v", stepKinds(steps), want)
	}

	// record_branch_recipe carries the whole declaration: the branch row, its
	// branch_state seed, and the fork/GC-cutoff interlock commit together, so a
	// rejected fork leaves no orphan row. There is deliberately no separate
	// create_branch step on this path.
	var rec RecordBranchRecipeArgs
	decodeArgs(t, stepByKind(t, steps, StepRecordBranchRecipe), &rec)
	if rec.ClusterID != clusterID {
		t.Errorf("recipe cluster = %s, want %s", rec.ClusterID, clusterID)
	}
	if rec.BranchID == uuid.Nil || rec.BranchID == parentID {
		t.Errorf("branch id = %s, want a fresh id distinct from the parent", rec.BranchID)
	}
	if rec.BranchName != "feature" {
		t.Errorf("recipe branch name = %q, want feature", rec.BranchName)
	}
	if rec.AncestorBranchID == nil || *rec.AncestorBranchID != parentID {
		t.Errorf("branch recipe ancestor = %v, want the parent %s", rec.AncestorBranchID, parentID)
	}
	if rec.StartLSN != "0/16ABCD0" {
		t.Errorf("branch recipe start_lsn = %q, want the fork lsn", rec.StartLSN)
	}

	var ep CreateEndpointArgs
	decodeArgs(t, stepByKind(t, steps, StepCreateEndpoint), &ep)
	if ep.BranchID != rec.BranchID {
		t.Errorf("endpoint targets %s, want the branch %s", ep.BranchID, rec.BranchID)
	}
}

func TestServiceCreateEndpointDefaultsRole(t *testing.T) {
	store := NewMemStore()
	svc := NewService(store)
	clusterID, branchID := uuid.New(), uuid.New()

	// Role unset defaults to read_write.
	tk, err := svc.CreateEndpoint(t.Context(), CreateEndpointInput{ClusterID: clusterID, BranchID: branchID, PgVersion: 18})
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	steps, _ := store.Steps(t.Context(), tk.ID)
	if tk.Kind != KindCreateEndpoint || len(steps) != 1 || steps[0].Kind != StepCreateEndpoint {
		t.Fatalf("task = kind %q with steps %v, want one create_endpoint step", tk.Kind, stepKinds(steps))
	}
	var ep CreateEndpointArgs
	decodeArgs(t, steps[0], &ep)
	if ep.Role != "read_write" {
		t.Errorf("default role = %q, want read_write", ep.Role)
	}

	tk2, _ := svc.CreateEndpoint(t.Context(), CreateEndpointInput{ClusterID: clusterID, BranchID: branchID, Role: "read_only", PgVersion: 18})
	steps2, _ := store.Steps(t.Context(), tk2.ID)
	var ep2 CreateEndpointArgs
	decodeArgs(t, steps2[0], &ep2)
	if ep2.Role != "read_only" {
		t.Errorf("role = %q, want read_only", ep2.Role)
	}
}

func TestServiceDeleteEndpoint(t *testing.T) {
	store := NewMemStore()
	svc := NewService(store)
	epID := uuid.New()
	tk, err := svc.DeleteEndpoint(t.Context(), DeleteEndpointInput{ClusterID: uuid.New(), EndpointID: epID})
	if err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}
	steps, _ := store.Steps(t.Context(), tk.ID)
	if tk.Kind != KindDeleteEndpoint || len(steps) != 1 || steps[0].Kind != StepDeleteEndpoint {
		t.Fatalf("task = kind %q with steps %v, want one delete_endpoint step", tk.Kind, stepKinds(steps))
	}
	var del DeleteEndpointArgs
	decodeArgs(t, steps[0], &del)
	if del.EndpointID != epID {
		t.Errorf("delete args endpoint = %s, want %s", del.EndpointID, epID)
	}
}

func TestServiceDeleteBranch(t *testing.T) {
	store := NewMemStore()
	svc := NewService(store)
	clusterID, branchID := uuid.New(), uuid.New()
	tk, err := svc.DeleteBranch(t.Context(), DeleteBranchInput{PoolID: uuid.New(), ClusterID: clusterID, BranchID: branchID})
	if err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
	steps, _ := store.Steps(t.Context(), tk.ID)
	if tk.Kind != KindDeleteBranch || len(steps) != 1 || steps[0].Kind != StepDeleteBranch {
		t.Fatalf("task = kind %q with steps %v, want one delete_branch step", tk.Kind, stepKinds(steps))
	}
	var del DeleteBranchArgs
	decodeArgs(t, steps[0], &del)
	if del.ClusterID != clusterID || del.BranchID != branchID {
		t.Errorf("delete_branch args = %+v, want cluster=%s branch=%s", del, clusterID, branchID)
	}
	if tk.ClusterID == nil || *tk.ClusterID != clusterID {
		t.Errorf("task ClusterID = %v, want %s", tk.ClusterID, clusterID)
	}
}

func TestServiceDeleteCluster(t *testing.T) {
	store := NewMemStore()
	svc := NewService(store)
	clusterID := uuid.New()
	tk, err := svc.DeleteCluster(t.Context(), DeleteClusterInput{PoolID: uuid.New(), ClusterID: clusterID})
	if err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	steps, _ := store.Steps(t.Context(), tk.ID)
	if tk.Kind != KindDeleteCluster || len(steps) != 1 || steps[0].Kind != StepDeleteCluster {
		t.Fatalf("task = kind %q with steps %v, want one delete_cluster step", tk.Kind, stepKinds(steps))
	}
	var del DeleteClusterArgs
	decodeArgs(t, steps[0], &del)
	if del.ClusterID != clusterID {
		t.Errorf("delete_cluster args cluster = %s, want %s", del.ClusterID, clusterID)
	}
}

func TestMemStoreList(t *testing.T) {
	store := NewMemStore()
	// A controllable clock so created_at differs per task and the DESC ordering is
	// deterministic without relying on the id tiebreak.
	base := time.Unix(1_700_000_000, 0).UTC()
	tick := 0
	store.now = func() time.Time {
		tick++
		return base.Add(time.Duration(tick) * time.Second)
	}
	svc := NewService(store)
	clusterA, clusterB := uuid.New(), uuid.New()

	// Three tasks, created oldest-first: A1, A2 (cluster A), B1 (cluster B).
	a1, _ := svc.DeleteCluster(t.Context(), DeleteClusterInput{ClusterID: clusterA})
	a2, _ := svc.DeleteBranch(t.Context(), DeleteBranchInput{ClusterID: clusterA, BranchID: uuid.New()})
	b1, _ := svc.DeleteCluster(t.Context(), DeleteClusterInput{ClusterID: clusterB})

	// Unfiltered: newest-first (b1, a2, a1), total 3.
	got, total, err := svc.List(t.Context(), TaskFilter{}, 10, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	wantOrder := []uuid.UUID{b1.ID, a2.ID, a1.ID}
	for i, w := range wantOrder {
		if got[i].ID != w {
			t.Fatalf("order[%d] = %s, want %s (created_at DESC)", i, got[i].ID, w)
		}
	}

	// Pagination: limit 2 / offset 1 -> a2, a1.
	page, total, _ := svc.List(t.Context(), TaskFilter{}, 2, 1)
	if total != 3 || len(page) != 2 || page[0].ID != a2.ID || page[1].ID != a1.ID {
		t.Fatalf("page = %v (total %d), want [a2 a1] total 3", taskIDs(page), total)
	}

	// Filter by cluster A: a2, a1 only.
	byCluster, total, _ := svc.List(t.Context(), TaskFilter{ClusterID: &clusterA}, 10, 0)
	if total != 2 || len(byCluster) != 2 {
		t.Fatalf("cluster filter total = %d (got %d rows), want 2", total, len(byCluster))
	}

	// Filter by kind: only the two delete_cluster tasks (a1, b1).
	byKind, total, _ := svc.List(t.Context(), TaskFilter{Kind: KindDeleteCluster}, 10, 0)
	if total != 2 {
		t.Fatalf("kind filter total = %d, want 2", total)
	}
	for _, tk := range byKind {
		if tk.Kind != KindDeleteCluster {
			t.Errorf("kind filter returned a %q task", tk.Kind)
		}
	}

	// Filter by status: all are pending; a failed filter matches none.
	none, total, _ := svc.List(t.Context(), TaskFilter{Status: StatusFailed}, 10, 0)
	if total != 0 || len(none) != 0 {
		t.Fatalf("failed-status filter = %d rows (total %d), want 0", len(none), total)
	}
}

func taskIDs(ts []*Task) []uuid.UUID {
	out := make([]uuid.UUID, len(ts))
	for i, t := range ts {
		out[i] = t.ID
	}
	return out
}
