package task

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// These tests validate the task Service (tasks.go): each CreateX method records
// the right ordered step kinds, generates and threads ids consistently across the
// steps (so a downstream step operates on the same project/branch/endpoint an
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

func TestServiceCreateProjectNoWriter(t *testing.T) {
	store := NewMemStore()
	svc := NewService(store)
	poolID := uuid.New()
	tk, err := svc.CreateProject(t.Context(), CreateProjectInput{PoolID: poolID, Name: "shop", PgVersion: 17})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if tk.Kind != KindCreateProject {
		t.Errorf("kind = %q, want %q", tk.Kind, KindCreateProject)
	}
	steps, _ := store.Steps(t.Context(), tk.ID)
	want := []string{StepCreateProject, StepRecordBranchRecipe}
	if !equalKinds(stepKinds(steps), want) {
		t.Fatalf("steps = %v, want %v", stepKinds(steps), want)
	}

	var cp CreateProjectArgs
	decodeArgs(t, stepByKind(t, steps, StepCreateProject), &cp)
	if cp.PoolID != poolID || cp.Name != "shop" || cp.PgVersion != 17 {
		t.Errorf("create-project args = %+v, want pool=%s name=shop pgv=17", cp, poolID)
	}
	if cp.ProjectID == uuid.Nil || cp.BranchID == uuid.Nil {
		t.Error("project/branch ids were not generated")
	}

	var rec RecordBranchRecipeArgs
	decodeArgs(t, stepByKind(t, steps, StepRecordBranchRecipe), &rec)
	// The recipe step must target the same project+branch the create step makes.
	if rec.ProjectID != cp.ProjectID || rec.BranchID != cp.BranchID {
		t.Errorf("recipe ids (%s,%s) != create-project ids (%s,%s)", rec.ProjectID, rec.BranchID, cp.ProjectID, cp.BranchID)
	}
	if rec.AncestorBranchID != nil {
		t.Error("default-branch recipe has an ancestor, want nil")
	}
	if rec.StartLSN != "0/0" {
		t.Errorf("default-branch recipe start_lsn = %q, want 0/0", rec.StartLSN)
	}

	// The task targets the project it creates (stamped for the task API).
	if tk.ProjectID == nil || *tk.ProjectID != cp.ProjectID {
		t.Errorf("task ProjectID = %v, want %s", tk.ProjectID, cp.ProjectID)
	}
	// Params round-trip to the originating request.
	var got CreateProjectInput
	if err := json.Unmarshal(tk.Params, &got); err != nil || got.Name != "shop" || got.PoolID != poolID {
		t.Errorf("params round-trip = %+v (err %v), want name=shop pool=%s", got, err, poolID)
	}
}

func TestServiceCreateProjectWithWriter(t *testing.T) {
	store := NewMemStore()
	svc := NewService(store)
	tk, err := svc.CreateProject(t.Context(), CreateProjectInput{PoolID: uuid.New(), Name: "shop", PgVersion: 17, WithWriter: true})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	steps, _ := store.Steps(t.Context(), tk.ID)
	want := []string{StepCreateProject, StepRecordBranchRecipe, StepCreateEndpoint}
	if !equalKinds(stepKinds(steps), want) {
		t.Fatalf("steps = %v, want %v", stepKinds(steps), want)
	}

	var cp CreateProjectArgs
	decodeArgs(t, stepByKind(t, steps, StepCreateProject), &cp)
	var ep CreateEndpointArgs
	decodeArgs(t, stepByKind(t, steps, StepCreateEndpoint), &ep)
	if ep.ProjectID != cp.ProjectID || ep.BranchID != cp.BranchID {
		t.Errorf("endpoint targets (%s,%s), want the created branch (%s,%s)", ep.ProjectID, ep.BranchID, cp.ProjectID, cp.BranchID)
	}
	if ep.Role != "read_write" {
		t.Errorf("first endpoint role = %q, want read_write", ep.Role)
	}
	if ep.EndpointID == uuid.Nil {
		t.Error("endpoint id was not generated")
	}
	if ep.AncestorBranchID != nil {
		t.Error("a default-branch endpoint has an ancestor, want nil")
	}
}

func TestServiceBranch(t *testing.T) {
	store := NewMemStore()
	svc := NewService(store)
	poolID, projectID, parentID := uuid.New(), uuid.New(), uuid.New()
	tk, err := svc.Branch(t.Context(), BranchInput{
		PoolID: poolID, ProjectID: projectID, ParentBranchID: parentID,
		Name: "feature", ForkLSN: "0/16ABCD0", PgVersion: 17, WithWriter: true,
	})
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	if tk.Kind != KindBranch {
		t.Errorf("kind = %q, want %q", tk.Kind, KindBranch)
	}
	steps, _ := store.Steps(t.Context(), tk.ID)
	want := []string{StepCreateBranch, StepRecordBranchRecipe, StepCreateEndpoint}
	if !equalKinds(stepKinds(steps), want) {
		t.Fatalf("steps = %v, want %v", stepKinds(steps), want)
	}

	var br CreateBranchArgs
	decodeArgs(t, stepByKind(t, steps, StepCreateBranch), &br)
	if br.ParentBranchID != parentID || br.ProjectID != projectID || br.ForkLSN != "0/16ABCD0" {
		t.Errorf("branch args = %+v, want parent=%s project=%s fork=0/16ABCD0", br, parentID, projectID)
	}
	if br.BranchID == uuid.Nil || br.BranchID == parentID {
		t.Errorf("branch id = %s, want a fresh id distinct from the parent", br.BranchID)
	}

	var rec RecordBranchRecipeArgs
	decodeArgs(t, stepByKind(t, steps, StepRecordBranchRecipe), &rec)
	if rec.BranchID != br.BranchID {
		t.Errorf("recipe branch %s != created branch %s", rec.BranchID, br.BranchID)
	}
	if rec.AncestorBranchID == nil || *rec.AncestorBranchID != parentID {
		t.Errorf("branch recipe ancestor = %v, want the parent %s", rec.AncestorBranchID, parentID)
	}
	if rec.StartLSN != "0/16ABCD0" {
		t.Errorf("branch recipe start_lsn = %q, want the fork lsn", rec.StartLSN)
	}

	var ep CreateEndpointArgs
	decodeArgs(t, stepByKind(t, steps, StepCreateEndpoint), &ep)
	if ep.BranchID != br.BranchID {
		t.Errorf("endpoint targets %s, want the branch %s", ep.BranchID, br.BranchID)
	}
	if ep.AncestorBranchID == nil || *ep.AncestorBranchID != parentID || ep.AncestorLSN != "0/16ABCD0" {
		t.Errorf("branch endpoint ancestor = (%v, %q), want (parent %s, fork lsn)", ep.AncestorBranchID, ep.AncestorLSN, parentID)
	}
}

func TestServiceCreateEndpointDefaultsRoleAndPassesAncestor(t *testing.T) {
	store := NewMemStore()
	svc := NewService(store)
	projectID, branchID, ancestor := uuid.New(), uuid.New(), uuid.New()

	// Role unset defaults to read_write.
	tk, err := svc.CreateEndpoint(t.Context(), CreateEndpointInput{ProjectID: projectID, BranchID: branchID, PgVersion: 17})
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

	// A fork target passes the ancestor through.
	tk2, _ := svc.CreateEndpoint(t.Context(), CreateEndpointInput{ProjectID: projectID, BranchID: branchID, Role: "read_only", PgVersion: 17, AncestorBranchID: &ancestor, AncestorLSN: "0/9"})
	steps2, _ := store.Steps(t.Context(), tk2.ID)
	var ep2 CreateEndpointArgs
	decodeArgs(t, steps2[0], &ep2)
	if ep2.Role != "read_only" {
		t.Errorf("role = %q, want read_only", ep2.Role)
	}
	if ep2.AncestorBranchID == nil || *ep2.AncestorBranchID != ancestor || ep2.AncestorLSN != "0/9" {
		t.Errorf("ancestor = (%v,%q), want (%s, 0/9)", ep2.AncestorBranchID, ep2.AncestorLSN, ancestor)
	}
}

func TestServiceDeleteEndpoint(t *testing.T) {
	store := NewMemStore()
	svc := NewService(store)
	epID := uuid.New()
	tk, err := svc.DeleteEndpoint(t.Context(), DeleteEndpointInput{ProjectID: uuid.New(), EndpointID: epID})
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
	projectID, branchID := uuid.New(), uuid.New()
	tk, err := svc.DeleteBranch(t.Context(), DeleteBranchInput{PoolID: uuid.New(), ProjectID: projectID, BranchID: branchID})
	if err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
	steps, _ := store.Steps(t.Context(), tk.ID)
	if tk.Kind != KindDeleteBranch || len(steps) != 1 || steps[0].Kind != StepDeleteBranch {
		t.Fatalf("task = kind %q with steps %v, want one delete_branch step", tk.Kind, stepKinds(steps))
	}
	var del DeleteBranchArgs
	decodeArgs(t, steps[0], &del)
	if del.ProjectID != projectID || del.BranchID != branchID {
		t.Errorf("delete_branch args = %+v, want project=%s branch=%s", del, projectID, branchID)
	}
	if tk.ProjectID == nil || *tk.ProjectID != projectID {
		t.Errorf("task ProjectID = %v, want %s", tk.ProjectID, projectID)
	}
}

func TestServiceDeleteProject(t *testing.T) {
	store := NewMemStore()
	svc := NewService(store)
	projectID := uuid.New()
	tk, err := svc.DeleteProject(t.Context(), DeleteProjectInput{PoolID: uuid.New(), ProjectID: projectID})
	if err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	steps, _ := store.Steps(t.Context(), tk.ID)
	if tk.Kind != KindDeleteProject || len(steps) != 1 || steps[0].Kind != StepDeleteProject {
		t.Fatalf("task = kind %q with steps %v, want one delete_project step", tk.Kind, stepKinds(steps))
	}
	var del DeleteProjectArgs
	decodeArgs(t, steps[0], &del)
	if del.ProjectID != projectID {
		t.Errorf("delete_project args project = %s, want %s", del.ProjectID, projectID)
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
	projectA, projectB := uuid.New(), uuid.New()

	// Three tasks, created oldest-first: A1, A2 (project A), B1 (project B).
	a1, _ := svc.DeleteProject(t.Context(), DeleteProjectInput{ProjectID: projectA})
	a2, _ := svc.DeleteBranch(t.Context(), DeleteBranchInput{ProjectID: projectA, BranchID: uuid.New()})
	b1, _ := svc.DeleteProject(t.Context(), DeleteProjectInput{ProjectID: projectB})

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

	// Filter by project A: a2, a1 only.
	byProject, total, _ := svc.List(t.Context(), TaskFilter{ProjectID: &projectA}, 10, 0)
	if total != 2 || len(byProject) != 2 {
		t.Fatalf("project filter total = %d (got %d rows), want 2", total, len(byProject))
	}

	// Filter by kind: only the two delete_project tasks (a1, b1).
	byKind, total, _ := svc.List(t.Context(), TaskFilter{Kind: KindDeleteProject}, 10, 0)
	if total != 2 {
		t.Fatalf("kind filter total = %d, want 2", total)
	}
	for _, tk := range byKind {
		if tk.Kind != KindDeleteProject {
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
