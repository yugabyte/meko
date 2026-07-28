package store

import (
	"strings"
	"testing"

	"meko/mccp/internal/api"
)

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Acme Prod":     "acme-prod",
		"  Globex_EU  ": "globex-eu",
		"a/b/c":         "a-b-c",
		"AlreadyClean":  "alreadyclean",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHashTokenIsStableAndOpaque(t *testing.T) {
	const tok = "mko_enr_abc123"
	h1, h2 := HashToken(tok), HashToken(tok)
	if h1 != h2 {
		t.Fatalf("HashToken not stable: %q != %q", h1, h2)
	}
	if len(h1) != 64 { // hex sha-256
		t.Fatalf("HashToken length = %d, want 64", len(h1))
	}
	if strings.Contains(h1, tok) {
		t.Fatal("hash must not embed the plaintext token")
	}
	if HashToken("other") == h1 {
		t.Fatal("distinct tokens hashed to the same value")
	}
}

func TestNewTokenPrefixAndHashPairing(t *testing.T) {
	plain, hash := NewToken("mko_sts_")
	if !strings.HasPrefix(plain, "mko_sts_") {
		t.Fatalf("token %q missing prefix", plain)
	}
	if hash != HashToken(plain) {
		t.Fatal("returned hash does not match HashToken(plain)")
	}
	// two mints are unique
	if p2, _ := NewToken("mko_sts_"); p2 == plain {
		t.Fatal("NewToken returned a duplicate token")
	}
}

func TestActionLifecycleTransitions(t *testing.T) {
	m := NewMemory()
	_ = m.SeedEnrollment("acme", "cloud", "t")

	steps := []struct {
		action string
		want   api.EnvState
	}{
		{"suspend", api.StateSuspended},
		{"resume", api.StateReady},
		{"drain", api.StateDraining},
		{"deregister", api.StateDeregistered},
	}
	for _, s := range steps {
		ok, err := m.Action("acme", s.action)
		if err != nil || !ok {
			t.Fatalf("Action(%q): ok=%v err=%v", s.action, ok, err)
		}
		e, _, _ := m.Get("acme")
		if e.State != s.want {
			t.Errorf("after %q state = %s, want %s", s.action, e.State, s.want)
		}
	}

	// unknown env → ok=false, no error
	if ok, err := m.Action("ghost", "suspend"); ok || err != nil {
		t.Fatalf("Action on unknown env = ok:%v err:%v, want ok:false err:nil", ok, err)
	}

	// unknown action is a no-op that still reports the env exists
	m.SetState("acme", api.StateReady)
	if ok, _ := m.Action("acme", "bogus"); !ok {
		t.Fatal("known env with unknown action should return ok=true")
	}
	if e, _, _ := m.Get("acme"); e.State != api.StateReady {
		t.Fatalf("unknown action changed state to %s", e.State)
	}
}

func TestSeedIfEmptyIsIdempotent(t *testing.T) {
	m := NewMemory()
	if err := m.SeedIfEmpty(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	first, _ := m.List()
	if len(first) == 0 {
		t.Fatal("SeedIfEmpty produced no environments")
	}
	// a second call must not duplicate or overwrite
	if err := m.SeedIfEmpty(); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	second, _ := m.List()
	if len(second) != len(first) {
		t.Fatalf("SeedIfEmpty not idempotent: %d then %d", len(first), len(second))
	}
}

func TestListSortsLiveFirstThenByID(t *testing.T) {
	m := NewMemory()
	_ = m.SeedEnrollment("zeta", "cloud", "z")  // not live
	_ = m.SeedEnrollment("alpha", "cloud", "a") // not live
	_ = m.SeedEnrollment("bravo", "cloud", "b") // will be live
	_, _, _, _ = m.EnrollWithToken("b", "cloud", api.ClusterFacts{})

	got, _ := m.List()
	if len(got) != 3 {
		t.Fatalf("List len = %d, want 3", len(got))
	}
	// live env sorts first
	if got[0].ID != "bravo" || !got[0].Live {
		t.Fatalf("first env = %s (live=%v), want bravo live", got[0].ID, got[0].Live)
	}
	// remaining non-live envs are sorted by ID
	if got[1].ID != "alpha" || got[2].ID != "zeta" {
		t.Fatalf("non-live order = [%s %s], want [alpha zeta]", got[1].ID, got[2].ID)
	}
}

func TestDatapacksFilterByEnv(t *testing.T) {
	m := NewMemory()
	_ = m.SeedEnrollment("one", "cloud", "1")
	_ = m.SeedEnrollment("two", "cloud", "2")
	_, _ = m.ApplyStatus("one", api.StatusReport{DatapackPhases: []api.DatapackPhase{
		{ID: "dp-b", Name: "beta", Phase: "Active", DB: "dp_beta"},
		{ID: "dp-a", Name: "alpha", Phase: "Provisioning"},
	}})
	_, _ = m.ApplyStatus("two", api.StatusReport{DatapackPhases: []api.DatapackPhase{
		{ID: "dp-c", Name: "gamma", Phase: "Active"},
	}})

	// filtered to one env
	one, _ := m.Datapacks("one")
	if len(one) != 2 {
		t.Fatalf("env=one datapacks = %d, want 2", len(one))
	}
	// sorted by name → alpha before beta
	if one[0]["name"] != "alpha" || one[1]["name"] != "beta" {
		t.Fatalf("datapacks not name-sorted: %v", one)
	}
	if one[1]["db"] != "dp_beta" {
		t.Fatalf("beta db = %v, want dp_beta", one[1]["db"])
	}

	// unfiltered → all envs
	all, _ := m.Datapacks("")
	if len(all) != 3 {
		t.Fatalf("all datapacks = %d, want 3", len(all))
	}
}
