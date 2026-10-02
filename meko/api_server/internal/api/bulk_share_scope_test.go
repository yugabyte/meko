package api_test

import (
	"bytes"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"meko/api_server/internal/api"
	"meko/api_server/internal/auth"
)

// POST /datapacks/:id/bulk-share moved onto the PAT-accepting group when the
// seven share routes did (MEKO-763), so a key can now drive a batch that
// provisions Cognito users and mails temporary passwords. The scope guard is
// the only thing standing between a read-only key and that, and Echo's routing
// table carries no middleware information — TestRouter_KBDocumentRoutesRegistered
// says as much — so the wiring is only observable by driving a real request
// through the real router.
//
// Dev auth cannot express this: with no IdP configured `dpDirect` IS
// `datapackGroup` and DevAuth injects pat_scopes=["*"], which RequireScope
// passes unconditionally. The test therefore sets Cognito coordinates so the
// PAT branch is wired, and authenticates with a PAT — a path that never reaches
// Cognito, because a meko PAT is issued and validated by this server alone.

const (
	// A pool/client that no request in this test reaches. They exist only to
	// satisfy `cognitoEnabled`, which is what splits dpDirect from the JWT-only
	// group; PAT validation is a local DB lookup.
	testCognitoRegion = "us-west-2"
	testCognitoPool   = "us-west-2_apitest"
	testCognitoClient = "apitestclient"
)

// openSplitTestDBs opens the two databases this server runs on: the datapack
// data plane and meko_system. They are separate DATABASES on the dev cluster,
// not two schemas in one, so the package's single-DSN openTestDB (which points
// at `yugabyte`) can never see meko_system.users and skips.
func openSplitTestDBs(t *testing.T) (dataDB, systemDB *sql.DB) {
	t.Helper()

	open := func(dsn, label string) *sql.DB {
		db, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Skipf("test database unavailable: open %s: %v", label, err)
		}
		if err := db.Ping(); err != nil {
			t.Skipf("test database unavailable: ping %s: %v", label, err)
		}
		return db
	}

	const base = "postgresql://localhost:5433/%s?user=yugabyte&password=yugabyte&sslmode=disable"
	dataDB = open(fmt.Sprintf(base, "yugabyte"), "yugabyte")
	systemDB = open(fmt.Sprintf(base, "meko_system"), "meko_system")

	var exists bool
	if err := systemDB.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		                WHERE table_schema = 'meko_system' AND table_name = 'users')`,
	).Scan(&exists); err != nil {
		t.Skipf("test database unavailable: schema probe: %v", err)
	}
	if !exists {
		t.Skip("meko_system database missing its schema; run migrations first")
	}
	return dataDB, systemDB
}

// seedPATUser creates a throwaway user plus one PAT per scope set, and returns
// the plaintext tokens. Registered for cleanup so a failed run leaves nothing.
func seedPATUser(t *testing.T, db *sql.DB, scopeSets map[string][]string) (map[string]string, string) {
	t.Helper()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	email := "bulkshare-scope-" + suffix + "@example.invalid"

	var userID string
	err := db.QueryRow(
		`INSERT INTO meko_system.users
		     (user_email, user_first_name, user_last_name, cognito_sub, email_verified_at)
		 VALUES ($1, 'Scope', 'Test', $2, now())
		 RETURNING user_id::text`,
		email, "scope-test-"+suffix,
	).Scan(&userID)
	if err != nil {
		t.Skipf("cannot seed test user (is this a writable dev database?): %v", err)
	}

	tokens := make(map[string]string, len(scopeSets))
	for label, scopes := range scopeSets {
		// Use the real generator rather than a hand-built string: the body must
		// be in the lowercased base32 alphabet or IsPATFormat rejects it and the
		// bearer falls through to the JWT validator, which 401s before the scope
		// guard is ever reached.
		plaintext, prefix, hash, err := auth.GeneratePAT()
		if err != nil {
			t.Fatalf("GeneratePAT: %v", err)
		}
		if _, err := db.Exec(
			`INSERT INTO meko_system.mcp_personal_access_tokens
			     (user_id, name, token_prefix, token_hash, scopes, expires_at)
			 VALUES ($1::uuid, $2, $3, $4, $5, now() + interval '1 hour')`,
			userID, "scope-test-"+label, prefix, hash, pqTextArray(scopes),
		); err != nil {
			t.Fatalf("seed PAT %s: %v", label, err)
		}
		tokens[label] = plaintext
	}

	t.Cleanup(func() {
		if _, err := db.Exec(
			`DELETE FROM meko_system.mcp_personal_access_tokens WHERE user_id = $1::uuid`,
			userID,
		); err != nil {
			t.Logf("cleanup PATs: %v", err)
		}
		if _, err := db.Exec(
			`DELETE FROM meko_system.users WHERE user_id = $1::uuid`, userID,
		); err != nil {
			t.Logf("cleanup user: %v", err)
		}
	})

	return tokens, userID
}

// pqTextArray renders a Go slice as a Postgres text[] literal. lib/pq cannot
// bind []string directly and pq.Array is not imported elsewhere in this package.
func pqTextArray(vs []string) string {
	quoted := make([]string, len(vs))
	for i, v := range vs {
		quoted[i] = `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
	}
	return "{" + strings.Join(quoted, ",") + "}"
}

func postBulkShare(t *testing.T, srvURL, token string) (int, string) {
	t.Helper()

	body := bytes.NewBufferString(`{"emails":["nobody@example.invalid"],"grant":"viewer"}`)
	req, err := http.NewRequest(http.MethodPost,
		srvURL+"/api/v1/datapacks/00000000-0000-0000-0000-000000000000/bulk-share", body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST bulk-share: %v", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			t.Logf("close body: %v", closeErr)
		}
	}()

	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, buf.String()
}

func TestBulkShare_ScopeGate(t *testing.T) {
	dataDB, systemDB := openSplitTestDBs(t)
	// t.Cleanup, not defer: seedPATUser registers its row cleanup afterwards and
	// Cleanup runs LIFO, so the deletes happen while the handles are still open.
	t.Cleanup(func() {
		_ = dataDB.Close()
		_ = systemDB.Close()
	})

	tokens, _ := seedPATUser(t, systemDB, map[string][]string{
		"read":  {"read"},
		"write": {"write"},
	})

	router, _, err := api.NewRouter(api.ServerConfig{
		DB:              dataDB,
		SystemDB:        systemDB,
		CognitoRegion:   testCognitoRegion,
		CognitoPoolID:   testCognitoPool,
		CognitoClientID: testCognitoClient,
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	srv := httptest.NewServer(router)
	defer srv.Close()

	// A read-only key must be refused by RequireScope(ScopeWrite), before any
	// datapack lookup: this is the assertion the route move is worth.
	t.Run("read-only key is refused", func(t *testing.T) {
		status, body := postBulkShare(t, srv.URL, tokens["read"])
		if status != http.StatusForbidden {
			t.Fatalf("expected 403, got %d: %s", status, body)
		}
		if !strings.Contains(body, "INSUFFICIENT_SCOPE") {
			t.Errorf("expected INSUFFICIENT_SCOPE, got %s", body)
		}
	})

	// A write key must clear the scope guard. It then fails on the datapack
	// guards (the all-zero id is not a datapack this user can reach), which is
	// the point: the failure has moved past scope.
	t.Run("write key clears the scope guard", func(t *testing.T) {
		status, body := postBulkShare(t, srv.URL, tokens["write"])
		if status == http.StatusForbidden && strings.Contains(body, "INSUFFICIENT_SCOPE") {
			t.Fatalf("write key was refused for scope: %s", body)
		}
		if status == http.StatusOK {
			t.Fatalf("all-zero datapack must not succeed; guards did not run: %s", body)
		}
	})

	// An unknown key never reaches the scope check at all.
	t.Run("unknown key is unauthenticated", func(t *testing.T) {
		status, body := postBulkShare(t, srv.URL, "mko_tkn_doesnotexist")
		if status != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", status, body)
		}
	})
}
