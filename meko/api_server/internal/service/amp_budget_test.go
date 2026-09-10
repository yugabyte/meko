package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/repository"
)

// budgetController serves the list endpoints SumMilliCoresByOwner walks for a
// single pool "p": one cluster "c1" → one branch "b1" → the given endpoints.
func budgetController(t *testing.T, endpoints []AMPEndpoint) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/storage-pools/p/clusters", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ampClusterList{Clusters: []AMPCluster{{ID: "c1"}}})
	})
	mux.HandleFunc("/api/v1/storage-pools/p/clusters/c1/branches", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ampBranchList{Branches: []AMPBranch{{BranchID: "b1"}}})
	})
	mux.HandleFunc("/api/v1/storage-pools/p/clusters/c1/branches/b1/endpoints", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ampEndpointList{Endpoints: endpoints})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// expectSum queues the two DB reads SumMilliCoresByOwner makes, in order: the
// owner's ready pools, then the controller registration (which client() reads).
func expectSum(mock sqlmock.Sqlmock, baseURL string) {
	mock.ExpectQuery(`FROM   meko_system.amp_datapack_pool`).
		WithArgs("u1").
		WillReturnRows(sqlmock.NewRows(
			[]string{"datapack_id", "shared_pool_id", "owner_user_id", "status", "created_at", "updated_at"}).
			AddRow("dp1", "p", "u1", "ready", time.Time{}, time.Time{}))
	mock.ExpectQuery(`FROM   meko_system.amp_registration`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"name", "base_url", "username", "password", "created_at", "updated_at"}).
			AddRow("amp", baseURL, "admin", "admin", time.Time{}, time.Time{}))
}

// A pool the controller can't list must be SKIPPED, not fatal: one stale pool
// must never block budget/enforcement for the user's other healthy datapacks.
func TestSumMilliCoresByOwner_SkipsUnreachablePool(t *testing.T) {
	srv := budgetController(t, []AMPEndpoint{{ID: "e1", Resources: &AMPResources{CPU: "500m"}}})
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	// Two ready pools: "p" is served (500m); "q" has no route on the controller,
	// so ListClusters errors — it must be skipped, not abort the whole sum.
	mock.ExpectQuery(`FROM   meko_system.amp_datapack_pool`).
		WithArgs("u1").
		WillReturnRows(sqlmock.NewRows(
			[]string{"datapack_id", "shared_pool_id", "owner_user_id", "status", "created_at", "updated_at"}).
			AddRow("dp1", "p", "u1", "ready", time.Time{}, time.Time{}).
			AddRow("dp2", "q", "u1", "ready", time.Time{}, time.Time{}))
	mock.ExpectQuery(`FROM   meko_system.amp_registration`).
		WillReturnRows(sqlmock.NewRows(
			[]string{"name", "base_url", "username", "password", "created_at", "updated_at"}).
			AddRow("amp", srv.URL, "admin", "admin", time.Time{}, time.Time{}))
	svc := NewAMPService(repository.NewAMPRepo(db), repository.NewAMPRegistrationRepo(db))

	total, err := svc.SumMilliCoresByOwner(context.Background(), "u1")
	if err != nil {
		t.Fatalf("want no error (unreachable pool skipped), got %v", err)
	}
	if total != 500 {
		t.Errorf("total = %d, want 500 (q skipped, p counted)", total)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// CoreBudgetExceeded (create / add-compute, no credit) sums every endpoint the
// owner runs (here a single 500m writer) and compares used + add against the cap.
func TestCoreBudgetExceeded(t *testing.T) {
	srv := budgetController(t, []AMPEndpoint{{ID: "e1", Resources: &AMPResources{CPU: "500m"}}})

	cases := []struct {
		name         string
		limit, add   int64
		want         bool
		expectFanout bool
	}{
		// used = 500m already reserved.
		{"free: adding a 0.5-core writer overshoots", 500, 500, true, true},
		{"free: exactly at the cap is not over", 500, 0, false, true},
		{"standard: room for another 2 cores? no", 2000, 2000, true, true},
		{"unlimited tier short-circuits before any fan-out", TierUnlimited, 8000, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer db.Close()
			if tc.expectFanout {
				expectSum(mock, srv.URL)
			}
			svc := NewAMPService(repository.NewAMPRepo(db), repository.NewAMPRegistrationRepo(db))

			got, err := svc.CoreBudgetExceeded(context.Background(), "u1", tc.limit, tc.add)
			if err != nil {
				t.Fatalf("CoreBudgetExceeded: %v", err)
			}
			if got != tc.want {
				t.Errorf("exceeded = %v, want %v", got, tc.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet DB expectations: %v", err)
			}
		})
	}
}

// ResizeCoreBudgetExceeded credits the endpoint's CURRENT size, captured in the
// SAME fan-out (no separate GetCompute that could fail open). Here e1 is the
// owner's only endpoint at 2 cores, so used=2000 and credit=2000 both come from
// that one walk — the check reduces to (new vs cap): a shrink passes, and a
// GROWTH beyond the cap is still blocked (the loophole a fail-open credit read
// would have opened).
func TestResizeCoreBudgetExceeded(t *testing.T) {
	srv := budgetController(t, []AMPEndpoint{{ID: "e1", Resources: &AMPResources{CPU: "2"}}})

	cases := []struct {
		name            string
		limit, newMilli int64
		want            bool
		expectFanout    bool
	}{
		{"shrinking e1 to 0.5 is allowed", 2000, 500, false, true},
		{"resizing e1 to exactly the cap is allowed", 2000, 2000, false, true},
		{"growing e1 past the cap is blocked", 2000, 3000, true, true},
		{"unlimited short-circuits before any fan-out", TierUnlimited, 9000, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer db.Close()
			if tc.expectFanout {
				expectSum(mock, srv.URL)
			}
			svc := NewAMPService(repository.NewAMPRepo(db), repository.NewAMPRegistrationRepo(db))

			got, err := svc.ResizeCoreBudgetExceeded(context.Background(), "u1", "e1", tc.limit, tc.newMilli)
			if err != nil {
				t.Fatalf("ResizeCoreBudgetExceeded: %v", err)
			}
			if got != tc.want {
				t.Errorf("exceeded = %v, want %v", got, tc.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet DB expectations: %v", err)
			}
		})
	}
}
