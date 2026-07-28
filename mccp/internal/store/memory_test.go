package store

import (
	"testing"
	"time"

	"meko/mccp/internal/api"
)

func TestEnrollTokenTTL(t *testing.T) {
	m := NewMemory()
	const tok = "mko_enr_testtoken"
	if err := m.SeedEnrollment("acme", "cloud", tok); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// wrong token is rejected
	if _, _, ok, _ := m.EnrollWithToken("wrong", "cloud", api.ClusterFacts{}); ok {
		t.Fatal("expected wrong token to be rejected")
	}

	// correct token enrolls and yields a status token
	e, status, ok, err := m.EnrollWithToken(tok, "cloud", api.ClusterFacts{NodeCount: 3, Region: "us-west1"})
	if err != nil || !ok {
		t.Fatalf("enroll failed: ok=%v err=%v", ok, err)
	}
	if e.ID != "acme" || status == "" {
		t.Fatalf("unexpected enroll result: id=%s status=%q", e.ID, status)
	}
	if e.State != api.StateAttesting {
		t.Errorf("state = %s, want ATTESTING", e.State)
	}

	// the issued status token validates; a bogus one / unknown env does not
	if ok, _ := m.ValidateStatusToken("acme", status); !ok {
		t.Error("valid status token rejected")
	}
	if ok, _ := m.ValidateStatusToken("acme", "bogus"); ok {
		t.Error("bogus status token accepted")
	}
	if ok, _ := m.ValidateStatusToken("nope", status); ok {
		t.Error("status token accepted for unknown env")
	}

	// token is reusable within its TTL (agent restart can re-enroll; mints a fresh status token)
	if _, _, ok, _ := m.EnrollWithToken(tok, "cloud", api.ClusterFacts{}); !ok {
		t.Fatal("expected re-enroll within TTL to succeed (reusable)")
	}

	// an expired token is rejected
	past := e.CreatedAt.Add(-time.Hour)
	m.envs["acme"].EnrollExpiresAt = &past
	if _, _, ok, _ := m.EnrollWithToken(tok, "cloud", api.ClusterFacts{}); ok {
		t.Fatal("expected expired token to be rejected")
	}
}

func TestDesiredResources(t *testing.T) {
	m := NewMemory()
	_ = m.SeedEnrollment("acme", "cloud", "t")

	want := []api.DesiredResource{{Kind: "datapack", Name: "alpha"}, {Kind: "datapack", Name: "beta"}}
	if ok, _ := m.SetDesiredResources("acme", want); !ok {
		t.Fatal("set desired failed")
	}
	got, _ := m.DesiredResources("acme")
	if len(got) != 2 || got[0].Name != "alpha" || got[0].Kind != "datapack" || got[1].Name != "beta" {
		t.Fatalf("desired = %v, want [alpha beta]", got)
	}

	// unknown env → not ok
	if ok, _ := m.SetDesiredResources("ghost", want); ok {
		t.Fatal("expected set desired on unknown env to fail")
	}
}

func TestApplyStatusUpdatesFacts(t *testing.T) {
	m := NewMemory()
	_ = m.SeedEnrollment("acme", "cloud", "t")
	_, _, _, _ = m.EnrollWithToken("t", "cloud", api.ClusterFacts{})

	ok, err := m.ApplyStatus("acme", api.StatusReport{
		EnvironmentState: api.StateReady,
		ClusterFacts:     api.ClusterFacts{NodeCount: 7, K8sVersion: "v1.34"},
		DatapackPhases:   []api.DatapackPhase{{ID: "yb-a", Name: "yb-a", Phase: "Active"}},
	})
	if err != nil || !ok {
		t.Fatalf("apply status: ok=%v err=%v", ok, err)
	}
	e, _, _ := m.Get("acme")
	if e.State != api.StateReady || e.NodeCount != 7 || e.Datapacks != 1 {
		t.Fatalf("env after status = %+v", e)
	}
}
