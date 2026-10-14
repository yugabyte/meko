package api_test

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/api"
)

// With MEKO_ENABLE_LINK_SHARING off the share-link routes are not registered;
// on, all four are.
func TestShareLinkRoutesFollowTheFlag(t *testing.T) {
	want := []string{
		"GET /api/v1/datapacks/:datapack_id/share-link",
		"PUT /api/v1/datapacks/:datapack_id/share-link",
		"GET /api/v1/share/:token",
		"POST /api/v1/share/:token/redeem",
	}
	for _, enabled := range []bool{false, true} {
		db, _, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		router, _, err := api.NewRouter(api.ServerConfig{DB: db, SystemDB: db, EnableLinkSharing: enabled})
		if err != nil {
			t.Fatalf("enabled=%v: NewRouter: %v", enabled, err)
		}
		have := map[string]bool{}
		for _, r := range router.Routes() {
			have[r.Method+" "+r.Path] = true
		}
		for _, route := range want {
			if have[route] != enabled {
				t.Errorf("enabled=%v: route %s registered=%v", enabled, route, have[route])
			}
		}
		db.Close()
	}
}
