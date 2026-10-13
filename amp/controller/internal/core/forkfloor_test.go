package core

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestParseLSN(t *testing.T) {
	for in, want := range map[string]uint64{
		"0/0":               0,
		"0/16B3748":         0x16B3748,
		"0/16b3748":         0x16B3748,
		"1/0":               1 << 32,
		"FFFFFFFF/FFFFFFFF": 1<<64 - 1,
		"0/FF":              0xFF,
	} {
		got, err := ParseLSN(in)
		if err != nil || got != want {
			t.Errorf("ParseLSN(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0", "/0", "0/", "0/0/0", "123456789/0", "0/G", " 0/0", "-1/0", "+1/0"} {
		if got, err := ParseLSN(bad); err == nil {
			t.Errorf("ParseLSN(%q) = %d, want an error", bad, got)
		}
	}
	// What the floor check rests on: the order is numeric, not lexical.
	a, _ := ParseLSN("0/FF")
	b, _ := ParseLSN("0/100")
	if a >= b {
		t.Errorf("0/FF parsed as %d, not below 0/100 (%d)", a, b)
	}
}

func TestPasswordRequired(t *testing.T) {
	parent := uuid.New()
	for name, tc := range map[string]struct {
		b    Branch
		want bool
	}{
		"a root with no floor":             {Branch{}, true},
		"a root with a floor":              {Branch{ForkLSN: "0/0"}, false},
		"a fork":                           {Branch{ParentBranchID: &parent, ForkLSN: "0/16B3748"}, false},
		"a fork whose fork_lsn is missing": {Branch{ParentBranchID: &parent}, false},
	} {
		if got := tc.b.PasswordRequired(); got != tc.want {
			t.Errorf("%s: Branch.PasswordRequired = %v, want %v", name, got, tc.want)
		}
		if got := (Cluster{DefaultBranch: tc.b}).PasswordRequired(); got != tc.want {
			t.Errorf("%s: Cluster.PasswordRequired = %v, want %v", name, got, tc.want)
		}
	}
}

// memPool creates a pool for SeedBranch to stub a cluster under, which the
// addressed reads require.
func memPool(t *testing.T, m *MemBackend) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	u, err := m.CreateUser(ctx, false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	p, err := m.CreateStoragePool(ctx, u.ID, "pool-"+uuid.NewString())
	if err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}
	return p.ID
}

func TestMemRootForkLSN(t *testing.T) {
	ctx := context.Background()
	m := NewMemBackend()
	poolID, clusterID, rootID, forkID := memPool(t, m), uuid.New(), uuid.New(), uuid.New()
	m.SeedBranch(poolID, Branch{ClusterID: clusterID, BranchID: rootID, Name: "main", IsDefault: true})
	if err := m.EnsureBranch(ctx, rootID, clusterID, forkID, "dev", "0/100", 18); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}

	pending := func(id uuid.UUID) bool {
		t.Helper()
		p, err := m.RootForkLSNPending(ctx, id)
		if err != nil {
			t.Fatalf("RootForkLSNPending: %v", err)
		}
		return p
	}
	forkLSN := func(id uuid.UUID) string {
		t.Helper()
		b, err := m.GetBranch(ctx, poolID, clusterID, id)
		if err != nil {
			t.Fatalf("GetBranch: %v", err)
		}
		return b.ForkLSN
	}
	if !pending(rootID) || pending(forkID) || pending(uuid.New()) {
		t.Fatalf("pending: root %v, fork %v, absent %v; want true, false, false", pending(rootID), pending(forkID), pending(uuid.New()))
	}

	if err := m.SetRootForkLSN(ctx, rootID, "0/200"); err != nil {
		t.Fatalf("SetRootForkLSN: %v", err)
	}
	if err := m.SetRootForkLSN(ctx, rootID, "0/300"); err != nil {
		t.Fatalf("SetRootForkLSN(second): %v", err)
	}
	if got := forkLSN(rootID); got != "0/200" {
		t.Errorf("root floor = %q, want the first writer's 0/200", got)
	}
	if pending(rootID) {
		t.Error("root still pending after its floor was set")
	}
	if err := m.SetRootForkLSN(ctx, forkID, "0/999"); err != nil {
		t.Fatalf("SetRootForkLSN(fork): %v", err)
	}
	if got := forkLSN(forkID); got != "0/100" {
		t.Errorf("fork fork_lsn = %q, want 0/100 untouched", got)
	}
	if n, err := m.BackfillRootForkLSN(ctx); n != 0 || err != nil {
		t.Errorf("BackfillRootForkLSN = %d, %v; want 0, nil", n, err)
	}
}

func TestMemForkBelowFloor(t *testing.T) {
	ctx := context.Background()
	m := NewMemBackend()
	poolID, clusterID, rootID := memPool(t, m), uuid.New(), uuid.New()
	m.SeedBranch(poolID, Branch{ClusterID: clusterID, BranchID: rootID, Name: "main", IsDefault: true, ForkLSN: "0/100"})

	below := uuid.New()
	err := m.EnsureBranch(ctx, rootID, clusterID, below, "below", "0/FF", 18)
	if !errors.Is(err, ErrForkBelowFloor) {
		t.Fatalf("EnsureBranch below the floor = %v, want ErrForkBelowFloor", err)
	}
	if _, err := m.CreateBranch(ctx, poolID, clusterID, rootID, "below2", "0/FF"); !errors.Is(err, ErrForkBelowFloor) {
		t.Errorf("CreateBranch below the floor = %v, want ErrForkBelowFloor", err)
	}

	at := uuid.New()
	if err := m.EnsureBranch(ctx, rootID, clusterID, at, "at", "0/100", 18); err != nil {
		t.Fatalf("EnsureBranch at the floor: %v", err)
	}
	// A replay stays a no-op whatever the floor is now.
	m.SeedBranch(poolID, Branch{ClusterID: clusterID, BranchID: rootID, Name: "main", IsDefault: true, ForkLSN: "0/500"})
	if err := m.EnsureBranch(ctx, rootID, clusterID, at, "at", "0/100", 18); err != nil {
		t.Errorf("EnsureBranch replay after the floor rose: %v, want a no-op", err)
	}
}
