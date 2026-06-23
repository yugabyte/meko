package task

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// These tests validate the write-ahead plan builders (plan.go): each task kind
// produces the right ordered step kinds, all ids are generated and threaded
// consistently across the steps (so a step downstream operates on the same
// project/branch/endpoint the upstream step created), and the fork vs.
// default-branch distinction (ancestor wiring) is correct. A plan is pure data,
// so these run without any collaborators.

func stepKinds(steps []StepPlan) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Kind
	}
	return out
}

func stepByKind(t *testing.T, steps []StepPlan, kind string) StepPlan {
	t.Helper()
	for _, s := range steps {
		if s.Kind == kind {
			return s
		}
	}
	t.Fatalf("no %q step in plan (got %v)", kind, stepKinds(steps))
	return StepPlan{}
}

func decodeArgs(t *testing.T, sp StepPlan, dst any) {
	t.Helper()
	if err := json.Unmarshal(sp.Args, dst); err != nil {
		t.Fatalf("decode %q args: %v", sp.Kind, err)
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

func TestBuildCreateProjectNoWriter(t *testing.T) {
	poolID := uuid.New()
	p, err := BuildCreateProject(CreateProjectInput{PoolID: poolID, Name: "shop", PgVersion: 17})
	if err != nil {
		t.Fatalf("BuildCreateProject: %v", err)
	}
	if p.Kind != KindCreateProject {
		t.Errorf("kind = %q, want %q", p.Kind, KindCreateProject)
	}
	want := []string{StepCreateProject, StepRecordBranchRecipe}
	if !equalKinds(stepKinds(p.Steps), want) {
		t.Fatalf("steps = %v, want %v", stepKinds(p.Steps), want)
	}

	var cp CreateProjectArgs
	decodeArgs(t, stepByKind(t, p.Steps, StepCreateProject), &cp)
	if cp.PoolID != poolID || cp.Name != "shop" || cp.PgVersion != 17 {
		t.Errorf("create-project args = %+v, want pool=%s name=shop pgv=17", cp, poolID)
	}
	if cp.ProjectID == uuid.Nil || cp.BranchID == uuid.Nil {
		t.Error("project/branch ids were not generated")
	}

	var rec RecordBranchRecipeArgs
	decodeArgs(t, stepByKind(t, p.Steps, StepRecordBranchRecipe), &rec)
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
	if p.ProjectID == nil || *p.ProjectID != cp.ProjectID {
		t.Errorf("plan ProjectID = %v, want %s", p.ProjectID, cp.ProjectID)
	}
	// Params round-trip to the originating request.
	var got CreateProjectInput
	if err := json.Unmarshal(p.Params, &got); err != nil || got.Name != "shop" || got.PoolID != poolID {
		t.Errorf("params round-trip = %+v (err %v), want name=shop pool=%s", got, err, poolID)
	}
}

func TestBuildCreateProjectWithWriter(t *testing.T) {
	p, err := BuildCreateProject(CreateProjectInput{PoolID: uuid.New(), Name: "shop", PgVersion: 17, WithWriter: true})
	if err != nil {
		t.Fatalf("BuildCreateProject: %v", err)
	}
	want := []string{StepCreateProject, StepRecordBranchRecipe, StepCreateEndpoint}
	if !equalKinds(stepKinds(p.Steps), want) {
		t.Fatalf("steps = %v, want %v", stepKinds(p.Steps), want)
	}

	var cp CreateProjectArgs
	decodeArgs(t, stepByKind(t, p.Steps, StepCreateProject), &cp)
	var ep CreateEndpointArgs
	decodeArgs(t, stepByKind(t, p.Steps, StepCreateEndpoint), &ep)
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

func TestBuildBranch(t *testing.T) {
	poolID, projectID, parentID := uuid.New(), uuid.New(), uuid.New()
	p, err := BuildBranch(BranchInput{
		PoolID: poolID, ProjectID: projectID, ParentBranchID: parentID,
		Name: "feature", ForkLSN: "0/16ABCD0", PgVersion: 17, WithWriter: true,
	})
	if err != nil {
		t.Fatalf("BuildBranch: %v", err)
	}
	if p.Kind != KindBranch {
		t.Errorf("kind = %q, want %q", p.Kind, KindBranch)
	}
	want := []string{StepCreateBranch, StepRecordBranchRecipe, StepCreateEndpoint}
	if !equalKinds(stepKinds(p.Steps), want) {
		t.Fatalf("steps = %v, want %v", stepKinds(p.Steps), want)
	}

	var br CreateBranchArgs
	decodeArgs(t, stepByKind(t, p.Steps, StepCreateBranch), &br)
	if br.ParentBranchID != parentID || br.ProjectID != projectID || br.ForkLSN != "0/16ABCD0" {
		t.Errorf("branch args = %+v, want parent=%s project=%s fork=0/16ABCD0", br, parentID, projectID)
	}
	if br.BranchID == uuid.Nil || br.BranchID == parentID {
		t.Errorf("branch id = %s, want a fresh id distinct from the parent", br.BranchID)
	}

	var rec RecordBranchRecipeArgs
	decodeArgs(t, stepByKind(t, p.Steps, StepRecordBranchRecipe), &rec)
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
	decodeArgs(t, stepByKind(t, p.Steps, StepCreateEndpoint), &ep)
	if ep.BranchID != br.BranchID {
		t.Errorf("endpoint targets %s, want the branch %s", ep.BranchID, br.BranchID)
	}
	if ep.AncestorBranchID == nil || *ep.AncestorBranchID != parentID || ep.AncestorLSN != "0/16ABCD0" {
		t.Errorf("branch endpoint ancestor = (%v, %q), want (parent %s, fork lsn)", ep.AncestorBranchID, ep.AncestorLSN, parentID)
	}
}

func TestBuildCreateEndpointDefaultsRoleAndPassesAncestor(t *testing.T) {
	projectID, branchID, ancestor := uuid.New(), uuid.New(), uuid.New()

	// Role unset defaults to read_write.
	p, err := BuildCreateEndpoint(CreateEndpointInput{ProjectID: projectID, BranchID: branchID, PgVersion: 17})
	if err != nil {
		t.Fatalf("BuildCreateEndpoint: %v", err)
	}
	if p.Kind != KindCreateEndpoint || len(p.Steps) != 1 || p.Steps[0].Kind != StepCreateEndpoint {
		t.Fatalf("plan = kind %q with steps %v, want one create_endpoint step", p.Kind, stepKinds(p.Steps))
	}
	var ep CreateEndpointArgs
	decodeArgs(t, p.Steps[0], &ep)
	if ep.Role != "read_write" {
		t.Errorf("default role = %q, want read_write", ep.Role)
	}

	// A fork target passes the ancestor through.
	p2, _ := BuildCreateEndpoint(CreateEndpointInput{ProjectID: projectID, BranchID: branchID, Role: "read_only", PgVersion: 17, AncestorBranchID: &ancestor, AncestorLSN: "0/9"})
	var ep2 CreateEndpointArgs
	decodeArgs(t, p2.Steps[0], &ep2)
	if ep2.Role != "read_only" {
		t.Errorf("role = %q, want read_only", ep2.Role)
	}
	if ep2.AncestorBranchID == nil || *ep2.AncestorBranchID != ancestor || ep2.AncestorLSN != "0/9" {
		t.Errorf("ancestor = (%v,%q), want (%s, 0/9)", ep2.AncestorBranchID, ep2.AncestorLSN, ancestor)
	}
}

func TestBuildDeleteEndpoint(t *testing.T) {
	epID := uuid.New()
	p, err := BuildDeleteEndpoint(DeleteEndpointInput{ProjectID: uuid.New(), EndpointID: epID})
	if err != nil {
		t.Fatalf("BuildDeleteEndpoint: %v", err)
	}
	if p.Kind != KindDeleteEndpoint || len(p.Steps) != 1 || p.Steps[0].Kind != StepDeleteEndpoint {
		t.Fatalf("plan = kind %q with steps %v, want one delete_endpoint step", p.Kind, stepKinds(p.Steps))
	}
	var del DeleteEndpointArgs
	decodeArgs(t, p.Steps[0], &del)
	if del.EndpointID != epID {
		t.Errorf("delete args endpoint = %s, want %s", del.EndpointID, epID)
	}
}
