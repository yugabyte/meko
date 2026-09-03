package task

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// createWithID derives the YCP stamp rather than each planner assembling it, so a
// new planner cannot wire the task and forget the stamp. These tests pin that
// derivation and the two ways a mis-wired kind is refused at plan time.

func gen64(v int64) *int64 { return &v }

// newTestService is a Service over a throwaway store, for the cases that only
// need one to plan a task.
func newTestService() *Service {
	store := NewMemStore()
	return NewService(store, store.Subtasks())
}

func TestCreateDerivesYCPStampFromTheKind(t *testing.T) {
	cluster, endpoint, branch := uuid.New(), uuid.New(), uuid.New()

	cases := []struct {
		name         string
		plan         func(*Service) (*Task, error)
		wantResource string
		wantID       uuid.UUID
	}{
		{
			name: "resize endpoint",
			plan: func(s *Service) (*Task, error) {
				return s.ResizeEndpoint(nil, ResizeEndpointInput{
					ClusterID: cluster, EndpointID: endpoint, CPU: "4", Memory: "8Gi",
					YCPGeneration: gen64(6),
				})
			},
			wantResource: YCPResourceEndpoint,
			wantID:       endpoint,
		},
		{
			name: "add branch extensions",
			plan: func(s *Service) (*Task, error) {
				return s.AddBranchExtensions(nil, AddBranchExtensionsInput{
					ClusterID: cluster, BranchID: branch, Extensions: []string{"pgvector"},
					YCPGeneration: gen64(6),
				})
			},
			wantResource: YCPResourceBranch,
			wantID:       branch,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemStore()
			tk, err := tc.plan(NewService(store, store.Subtasks()))
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if tk.YCP == nil {
				t.Fatal("no YCP stamp derived")
			}
			if tk.YCP.Resource != tc.wantResource || tk.YCP.ID != tc.wantID || tk.YCP.Generation != 6 {
				t.Errorf("stamp = %+v, want {%s %s 6}", *tk.YCP, tc.wantResource, tc.wantID)
			}
		})
	}
}

// A caller that is not the reconcile agent — an operator, the UI — sends no
// generation, and its task must leave whatever the row already records alone.
func TestCreateOmitsYCPStampWithoutAGeneration(t *testing.T) {
	store := NewMemStore()
	svc := NewService(store, store.Subtasks())

	tk, err := svc.ResizeEndpoint(nil, ResizeEndpointInput{
		ClusterID: uuid.New(), EndpointID: uuid.New(), CPU: "4", Memory: "8Gi",
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if tk.YCP != nil {
		t.Errorf("stamp = %+v, want none for a caller with no generation", *tk.YCP)
	}
}

// The guard that makes the invariant hold for kinds nobody has written yet: a
// kind declaring a ycpResource whose input cannot carry a generation is refused on
// its FIRST call — in any unit test of that planner — instead of silently never
// stamping. The check runs even though this caller sends no generation, because it
// is the input type that has to be capable of carrying one.
func TestCreateRejectsAKindWhoseInputCannotCarryAGeneration(t *testing.T) {
	svc := newTestService()
	badKind := Kind{"upgrade_endpoint", YCPResourceEndpoint, "", nil}

	_, err := svc.create(nil, badKind, struct{ Foo string }{"bar"}, uuid.New(), uuid.New().String(), nil)
	if err == nil {
		t.Fatal("create = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "cannot carry a generation") {
		t.Errorf("err = %v, want it to name the missing generation carrier", err)
	}
}

// Likewise a kind naming a resource the worker cannot stamp: caught at plan time
// rather than as a task the worker keeps reclaiming.
func TestCreateRejectsUnknownYCPResource(t *testing.T) {
	svc := newTestService()
	badKind := Kind{"upgrade_datapack", "datapack", "", nil}

	_, err := svc.create(nil, badKind, ResizeEndpointInput{YCPGeneration: gen64(1)}, uuid.New(), uuid.New().String(), nil)
	if err == nil {
		t.Fatal("create = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "unknown ycpResource") {
		t.Errorf("err = %v, want it to name the unknown resource", err)
	}
}

// The stamp needs the resource's id, which is target_ref. A kind that declares a
// resource but plans no target_ref could not be stamped, so it is refused rather
// than losing the generation quietly.
func TestCreateRejectsAYCPStampWithNoTargetRef(t *testing.T) {
	svc := newTestService()

	_, err := svc.create(nil, KindResizeEndpoint, ResizeEndpointInput{YCPGeneration: gen64(1)}, uuid.New(), "", nil)
	if err == nil {
		t.Fatal("create = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "target_ref") {
		t.Errorf("err = %v, want it to name the missing target_ref", err)
	}
}

// add_branch_extensions used to pass the branch id where clusterID belongs and
// leave target_ref empty. target_ref is now the branch id, which is what both the
// first-writer-wins index and the derived stamp key on.
func TestAddBranchExtensionsTargetsTheBranch(t *testing.T) {
	store := NewMemStore()
	cluster, branch := uuid.New(), uuid.New()

	tk, err := NewService(store, store.Subtasks()).AddBranchExtensions(nil, AddBranchExtensionsInput{
		ClusterID: cluster, BranchID: branch, Extensions: []string{"pgvector"},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if tk.TargetRef == nil || *tk.TargetRef != branch.String() {
		t.Errorf("target_ref = %v, want the branch id %s", tk.TargetRef, branch)
	}
	if tk.ClusterID == nil || *tk.ClusterID != cluster {
		t.Errorf("cluster_id = %v, want the cluster id %s", tk.ClusterID, cluster)
	}
}

// The extensions interlock, the twin of the resize one. It exists because the
// agent now re-issues an update until the row confirms it, and the step redeploys
// every endpoint on the branch — two concurrent tasks would run concurrent
// upgrades of the same release.
func TestBranchExtensionsLock(t *testing.T) {
	branch, other := uuid.NewString(), uuid.NewString()
	spec := func(ref string) TaskSpec {
		r := ref
		return TaskSpec{
			Kind:      KindAddBranchExtensions,
			TargetRef: &r,
			Steps:     []StepSpec{{Kind: StepAddBranchExtensions}},
		}
	}

	t.Run("a concurrent add of the same branch is rejected", func(t *testing.T) {
		s := NewMemStore()
		if _, err := s.Create(t.Context(), spec(branch)); err != nil {
			t.Fatalf("first add: %v", err)
		}
		if _, err := s.Create(t.Context(), spec(branch)); !errors.Is(err, ErrBranchExtensionsInProgress) {
			t.Fatalf("second add err = %v, want ErrBranchExtensionsInProgress", err)
		}
	})

	t.Run("another branch is unaffected", func(t *testing.T) {
		s := NewMemStore()
		if _, err := s.Create(t.Context(), spec(branch)); err != nil {
			t.Fatalf("first add: %v", err)
		}
		if _, err := s.Create(t.Context(), spec(other)); err != nil {
			t.Fatalf("add on another branch = %v, want admitted", err)
		}
	})

	t.Run("a resize does not hold a branch", func(t *testing.T) {
		s := NewMemStore()
		ref := branch
		s.Seed(&Task{ID: uuid.New(), Kind: KindResizeEndpoint.String(), Status: StatusRunning, TargetRef: &ref}, nil)
		if _, err := s.Create(t.Context(), spec(branch)); err != nil {
			t.Fatalf("add alongside a resize on the same ref = %v, want admitted", err)
		}
	})

	t.Run("a terminal add releases the lock", func(t *testing.T) {
		s := NewMemStore()
		ref := branch
		s.Seed(&Task{ID: uuid.New(), Kind: KindAddBranchExtensions.String(), Status: StatusSucceeded, TargetRef: &ref}, nil)
		if _, err := s.Create(t.Context(), spec(branch)); err != nil {
			t.Fatalf("add after a terminal one = %v, want admitted", err)
		}
	})
}

// Every locked kind answers with its OWN conflict. The gate reads lockIndex, so
// the error has to ride on the same value — selecting it on any other axis means a
// second place to disagree, and a future locked kind reporting "a resize is
// already in progress" for a branch.
func TestEveryLockedKindHasItsOwnConflict(t *testing.T) {
	seen := map[error]string{}
	for _, k := range allKinds {
		if k.lockIndex == "" {
			if k.inProgress != nil {
				t.Errorf("%s has no lock but declares a conflict error", k)
			}
			continue
		}
		if k.inProgress == nil {
			t.Errorf("%s is first-writer-wins but declares no conflict error", k)
			continue
		}
		if other, dup := seen[k.inProgress]; dup {
			t.Errorf("%s and %s share a conflict error, so a 409 cannot say which", k, other)
		}
		seen[k.inProgress] = k.String()
	}
}
