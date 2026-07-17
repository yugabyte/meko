package main

import (
	"context"
	"testing"
	"time"

	"github.com/yugabyte/yb-amp/test/functional/framework/apps/cloud"
)

// reapRecorderVM is a cloud.VM double that records the reap calls `env reap`
// dispatches, so the command's wiring is tested without touching a real cloud.
type reapRecorderVM struct {
	byTag    []string
	staleFor []time.Duration
}

func (r *reapRecorderVM) RunInstances(context.Context, cloud.RunSpec) ([]cloud.Instance, error) {
	return nil, nil
}
func (r *reapRecorderVM) WaitRunning(context.Context, []string) error             { return nil }
func (r *reapRecorderVM) WaitSSH(context.Context, []cloud.Instance, string) error { return nil }
func (r *reapRecorderVM) Describe(context.Context, map[string]string) ([]cloud.Instance, error) {
	return nil, nil
}
func (r *reapRecorderVM) Terminate(context.Context, []string) error { return nil }
func (r *reapRecorderVM) ReapByTag(_ context.Context, tag string) error {
	r.byTag = append(r.byTag, tag)
	return nil
}
func (r *reapRecorderVM) ReapStale(_ context.Context, d time.Duration) error {
	r.staleFor = append(r.staleFor, d)
	return nil
}
func (r *reapRecorderVM) Connect(context.Context, cloud.Instance) (cloud.Conn, error) {
	return nil, nil
}
func (r *reapRecorderVM) Meta() cloud.Meta { return cloud.Meta{} }

// TestEnvReapDispatchesTagAndStale proves `ampqa env reap` reaches the cloud
// driver's ownership-tag and age-based safety nets — the recovery path for a
// killed/lost run's leaked infra that previously had no CLI at all.
func TestEnvReapDispatchesTagAndStale(t *testing.T) {
	rec := &reapRecorderVM{}
	orig := cloudVMForReap
	cloudVMForReap = func(string, cloud.ConfigGetter, cloud.SecretResolver) (cloud.VM, error) { return rec, nil }
	defer func() { cloudVMForReap = orig }()

	root := buildRoot()
	root.SetArgs([]string{"env", "reap", "--tag", "cafef00d", "--stale", "48h"})
	if err := root.Execute(); err != nil {
		t.Fatalf("env reap: %v", err)
	}
	if len(rec.byTag) != 1 || rec.byTag[0] != "cafef00d" {
		t.Fatalf("ReapByTag calls = %v, want [cafef00d]", rec.byTag)
	}
	if len(rec.staleFor) != 1 || rec.staleFor[0] != 48*time.Hour {
		t.Fatalf("ReapStale calls = %v, want [48h]", rec.staleFor)
	}
}

// TestEnvReapRequiresATarget: with neither --tag nor --stale there is nothing to
// reap, so the command is a usage error rather than a silent no-op.
func TestEnvReapRequiresATarget(t *testing.T) {
	root := buildRoot()
	root.SetArgs([]string{"env", "reap"})
	if err := root.Execute(); err == nil {
		t.Fatal("env reap with no --tag/--stale should be a usage error")
	}
}
