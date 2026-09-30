package core

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/db"
)

// TestSeedFleetVersion covers the startup seed: an empty fleet is at the pin
// whatever was recorded, a recorded value over a non-empty fleet is kept, and an
// unrecorded one is seeded from the endpoints — never from the pin, which would
// claim a roll that did not happen.
func TestSeedFleetVersion(t *testing.T) {
	for name, tc := range map[string]struct {
		pin      string
		recorded string
		versions []string // one endpoint per entry; "" records no version
		want     string   // the recorded fleet version after the seed; "" = none
	}{
		"empty fleet takes the pin":        {pin: "v2", want: "v2"},
		"empty fleet overrides a recorded": {pin: "v2", recorded: "v1", want: "v2"},
		"recorded is kept":                 {pin: "v2", recorded: "v1", versions: []string{"v2"}, want: "v1"},
		"unrecorded, one version":          {pin: "v2", versions: []string{"v1", "v1"}, want: "v1"},
		"unrecorded, two versions":         {pin: "v2", versions: []string{"v1", "v2"}, want: FleetVersionMixed},
		"unrecorded, a version missing":    {pin: "v2", versions: []string{"v1", ""}, want: FleetVersionMixed},
		"unrecorded, no versions at all":   {pin: "v2", versions: []string{"", ""}, want: FleetVersionMixed},
	} {
		t.Run(name, func(t *testing.T) {
			m := NewMemBackend()
			ctx := t.Context()
			clusterID, branchID := uuid.New(), uuid.New()
			m.SeedBranch(uuid.New(), Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})
			for i, v := range tc.versions {
				role := RoleReadOnly
				if i == 0 {
					role = RoleReadWrite
				}
				ep, err := m.CreateEndpoint(ctx, clusterID, branchID, role, nil)
				if err != nil {
					t.Fatalf("CreateEndpoint: %v", err)
				}
				if err := m.SetEndpointAddress(ctx, ep.ID, "yb-ep.endpoints.svc:55433", ""); err != nil {
					t.Fatalf("SetEndpointAddress: %v", err)
				}
				if v != "" {
					if err := m.SetEndpointVersion(ctx, ep.ID, v); err != nil {
						t.Fatalf("SetEndpointVersion: %v", err)
					}
				}
			}
			if tc.recorded != "" {
				if err := m.SetFleetVersion(ctx, tc.recorded); err != nil {
					t.Fatal(err)
				}
			}

			if err := SeedFleetVersion(ctx, m, tc.pin); err != nil {
				t.Fatalf("SeedFleetVersion: %v", err)
			}
			if got, _ := m.FleetVersion(ctx); got != tc.want {
				t.Errorf("fleet version = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEffectiveFleetVersion: an empty fleet reads at the pin whatever is recorded;
// a fleet with endpoints reads its record.
func TestEffectiveFleetVersion(t *testing.T) {
	ctx := t.Context()
	m := NewMemBackend()
	if err := m.SetFleetVersion(ctx, "v1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := EffectiveFleetVersion(ctx, m, "v2"); got != "v2" {
		t.Errorf("empty fleet = %q, want the pin v2", got)
	}
	clusterID, branchID := uuid.New(), uuid.New()
	m.SeedBranch(uuid.New(), Branch{ClusterID: clusterID, BranchID: branchID, Name: "main", PgVersion: 18, IsDefault: true})
	ep, err := m.CreateEndpoint(ctx, clusterID, branchID, RoleReadWrite, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetEndpointAddress(ctx, ep.ID, "yb-ep.endpoints.svc:55433", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := EffectiveFleetVersion(ctx, m, "v2"); got != "v1" {
		t.Errorf("fleet with an endpoint = %q, want the record v1", got)
	}

	// The rollback case: the record equals the pin, but an endpoint created while
	// another version was pinned is not on it. Believing the record would refuse
	// the roll that moves it.
	if err := m.SetEndpointVersion(ctx, ep.ID, "v2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := EffectiveFleetVersion(ctx, m, "v1"); got != FleetVersionMixed {
		t.Errorf("record at the pin with an endpoint elsewhere = %q, want %q", got, FleetVersionMixed)
	}
	if err := m.SetEndpointVersion(ctx, ep.ID, "v1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := EffectiveFleetVersion(ctx, m, "v1"); got != "v1" {
		t.Errorf("record at the pin with every endpoint on it = %q, want v1", got)
	}
}

// TestPgFleetVersion covers the real statements: a miss is ("", nil), and
// SetFleetVersion is an upsert. Set AMP_TEST_DATABASE_URL to run.
func TestPgFleetVersion(t *testing.T) {
	url := os.Getenv("AMP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AMP_TEST_DATABASE_URL (e.g. postgres://yugabyte@host:5433/yb_storage?sslmode=disable) to run")
	}
	const schema = "controller"
	provCtx, provCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer provCancel()
	scratchURL, drop, err := db.ScratchMigrated(provCtx, url, "amp_test_core_fleet_version", schema)
	if err != nil {
		t.Fatalf("provision scratch database: %v", err)
	}
	t.Cleanup(drop)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, scratchURL, schema)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	b := NewPgBackend(pool)

	if v, err := b.FleetVersion(ctx); err != nil || v != "" {
		t.Fatalf("FleetVersion on a fresh schema = (%q, %v), want (\"\", nil)", v, err)
	}
	for _, want := range []string{"v1", "v2"} {
		if err := b.SetFleetVersion(ctx, want); err != nil {
			t.Fatalf("SetFleetVersion(%s): %v", want, err)
		}
		if v, err := b.FleetVersion(ctx); err != nil || v != want {
			t.Fatalf("FleetVersion = (%q, %v), want (%q, nil)", v, err, want)
		}
	}
}
