package tasks

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/env/reap"
	"github.com/yugabyte/meko/meko/test/functional/framework/env/registry"
	"github.com/yugabyte/meko/meko/test/functional/framework/task"
)

func TestTaskRegistryHasBuiltins(t *testing.T) {
	for _, name := range []string{"reap", "deploy-artifact"} {
		tk, ok := task.Default().Get(name)
		if !ok {
			t.Errorf("built-in task %q not registered", name)
			continue
		}
		if tk.Doc == "" {
			t.Errorf("task %q has no Doc", name)
		}
		if tk.Run == nil {
			t.Errorf("task %q has no Run", name)
		}
	}
}

func TestTaskListSortedForCLI(t *testing.T) {
	list := task.Default().List()
	for i := 1; i < len(list); i++ {
		if list[i-1].Name > list[i].Name {
			t.Fatalf("List not sorted: %q before %q", list[i-1].Name, list[i].Name)
		}
	}
}

func TestReapDryRunRemovesNothing(t *testing.T) {
	reg := registry.New(filepath.Join(t.TempDir(), "envs.json"))
	expired, err := reg.Add(registry.Entry{
		Name: "old", CreatedAt: time.Now().Add(-2 * time.Hour), TTL: time.Hour,
		Data: map[string]string{"reap_plan": `{"namespaces":[{"kind":"ybk8s","namespace":"mekoqa-yb-dry"}]}`},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	tornDown := false
	res, err := Reap(context.Background(), reg, ReapOptions{
		DryRun:  true,
		Execute: func(context.Context, *reap.Plan) error { tornDown = true; return nil },
	})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(res.Expired) != 1 || res.Expired[0] != expired.ID {
		t.Errorf("Expired = %v, want [%s]", res.Expired, expired.ID)
	}
	if len(res.Removed) != 0 {
		t.Errorf("dry run must not remove anything, got %v", res.Removed)
	}
	if tornDown {
		t.Error("dry run must not tear anything down")
	}
	_, ok, err := reg.Get(expired.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Error("dry run must leave the expired entry in place")
	}
}

// TestReapFreshEntrySurvives: only entries past their TTL are candidates.
func TestReapFreshEntrySurvives(t *testing.T) {
	reg := registry.New(filepath.Join(t.TempDir(), "envs.json"))
	fresh, err := reg.Add(registry.Entry{
		Name: "fresh-soak", Provider: "k8s",
		CreatedAt: time.Now(), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("Add fresh: %v", err)
	}
	res, err := Reap(context.Background(), reg, ReapOptions{})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(res.Expired) != 0 || len(res.Removed) != 0 {
		t.Errorf("Expired=%v Removed=%v, want neither for a fresh entry", res.Expired, res.Removed)
	}
	if _, ok, _ := reg.Get(fresh.ID); !ok {
		t.Error("fresh (non-expired) entry should not be reaped")
	}
}

// TestReapUnknownProviderRemovedWithoutTeardown: an entry recording no
// infrastructure handle at all has nothing to reclaim, so its stale record is
// dropped without invoking any teardown seam.
func TestReapUnknownProviderRemovedWithoutTeardown(t *testing.T) {
	reg := registry.New(filepath.Join(t.TempDir(), "envs.json"))
	expired, err := reg.Add(registry.Entry{
		Name: "manual-vm", Provider: "vm", CreatedAt: time.Now().Add(-2 * time.Hour), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	tornDown := false
	res, err := Reap(context.Background(), reg, ReapOptions{
		Execute:   func(context.Context, *reap.Plan) error { tornDown = true; return nil },
		CloudReap: func(context.Context, string, string) error { tornDown = true; return nil },
	})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != expired.ID {
		t.Errorf("Removed = %v, want [%s]", res.Removed, expired.ID)
	}
	if tornDown {
		t.Error("an entry with no infrastructure handle must not invoke a teardown seam")
	}
}

// TestReapExecutesRecordedPlan: a k8s/cloud entry that carries a reap.Plan has
// its plan EXECUTED (not silently removed), and on success the record is dropped.
func TestReapExecutesRecordedPlan(t *testing.T) {
	reg := registry.New(filepath.Join(t.TempDir(), "envs.json"))
	expired, err := reg.Add(registry.Entry{
		Name: "k8s-soak", Provider: "functional", Shape: "yb_k8s",
		CreatedAt: time.Now().Add(-2 * time.Hour), TTL: time.Hour,
		Data: map[string]string{"reap_plan": `{"namespaces":[{"kind":"ybk8s","namespace":"mekoqa-yb-abc"}]}`},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	var executed []string
	res, err := Reap(context.Background(), reg, ReapOptions{
		Execute: func(_ context.Context, p *reap.Plan) error {
			for _, n := range p.Namespaces {
				executed = append(executed, n.Namespace)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(executed) != 1 || executed[0] != "mekoqa-yb-abc" {
		t.Fatalf("plan executor ran for %v, want [mekoqa-yb-abc]", executed)
	}
	if len(res.Removed) != 1 || res.Removed[0] != expired.ID {
		t.Errorf("Removed = %v, want [%s] after a successful plan reap", res.Removed, expired.ID)
	}
}

// TestReapFailedPlanKeepsRecord: when the recorded plan's reap fails, the record
// is KEPT with a loud error, the leaked namespace/instances stay discoverable.
func TestReapFailedPlanKeepsRecord(t *testing.T) {
	reg := registry.New(filepath.Join(t.TempDir(), "envs.json"))
	expired, err := reg.Add(registry.Entry{
		Name: "k8s-soak", Provider: "functional", Shape: "yb_k8s",
		CreatedAt: time.Now().Add(-2 * time.Hour), TTL: time.Hour,
		Data: map[string]string{"reap_plan": `{"namespaces":[{"kind":"ybk8s","namespace":"mekoqa-yb-abc"}]}`},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	res, err := Reap(context.Background(), reg, ReapOptions{
		Execute: func(context.Context, *reap.Plan) error { return errAlwaysFails{} },
	})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if res.TeardownErrs[expired.ID] == nil {
		t.Errorf("expected a teardown error for the failed plan reap, got %v", res.TeardownErrs)
	}
	if len(res.Removed) != 0 {
		t.Errorf("a failed plan reap must KEEP the record, got Removed=%v", res.Removed)
	}
	if _, ok, _ := reg.Get(expired.ID); !ok {
		t.Error("entry with a failed plan reap must remain in the registry")
	}
}

// TestReapCloudTagEntry: a bare cloud entry (run_tag + cloud, no plan) is reaped
// via the cloud tag-reap seam, then removed.
func TestReapCloudTagEntry(t *testing.T) {
	reg := registry.New(filepath.Join(t.TempDir(), "envs.json"))
	expired, err := reg.Add(registry.Entry{
		Name: "vm-soak", Provider: "vm", Shape: "vm",
		CreatedAt: time.Now().Add(-2 * time.Hour), TTL: time.Hour,
		Data: map[string]string{"cloud": "aws", "run_tag": "cafef00d"},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	var reaped []string
	res, err := Reap(context.Background(), reg, ReapOptions{
		CloudReap: func(_ context.Context, cloudName, runTag string) error {
			reaped = append(reaped, cloudName+"/"+runTag)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != "aws/cafef00d" {
		t.Fatalf("cloud reaper ran for %v, want [aws/cafef00d]", reaped)
	}
	if len(res.Removed) != 1 || res.Removed[0] != expired.ID {
		t.Errorf("Removed = %v, want [%s] after a successful tag reap", res.Removed, expired.ID)
	}
}

type errAlwaysFails struct{}

func (errAlwaysFails) Error() string { return "teardown always fails" }
