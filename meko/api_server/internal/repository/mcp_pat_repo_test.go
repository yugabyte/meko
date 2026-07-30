package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

var mcpPATColumns = []string{
	"id", "user_id", "name", "token_prefix",
	"created_at", "last_used_at", "expires_at", "revoked_at", "scopes",
}

// scopesLit is the Postgres text wire-format for a TEXT[] value.
// sqlmock returns this as the column's driver.Value and lib/pq's
// Array Scanner parses it the same way it would a real result row.
func scopesLit(scopes ...string) string {
	if len(scopes) == 0 {
		return "{}"
	}
	out := "{"
	for i, s := range scopes {
		if i > 0 {
			out += ","
		}
		out += `"` + s + `"`
	}
	out += "}"
	return out
}

//nolint:unparam // userID and lastUsed kept for parity with repo column ordering; tests pass canonical values.
func newMCPPATRow(
	id, userID, name, prefix string,
	created time.Time,
	lastUsed, expires, revoked any,
	scopes string,
) *sqlmock.Rows {
	return sqlmock.NewRows(mcpPATColumns).AddRow(
		id, userID, name, prefix,
		created, lastUsed, expires, revoked, scopes,
	)
}

func TestMCPPATRepo_Create_ReturnsRecord(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now().UTC()
	expires := now.Add(90 * 24 * time.Hour)
	mock.ExpectQuery(`INSERT INTO meko_system\.mcp_personal_access_tokens`).
		WithArgs("user-1", "cursor", "mko_tkn_3KvL9X8z", "hash-abc",
			sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(newMCPPATRow(
			"pat-1", "user-1", "cursor", "mko_tkn_3KvL9X8z",
			now, nil, expires, nil, scopesLit("*"),
		))

	repo := NewMCPPATRepo(db)
	//nolint:gosec // sqlmock test fixture, not a real credential.
	rec, err := repo.Create(context.Background(), MCPPATCreateParams{
		UserID:      "user-1",
		Name:        "cursor",
		TokenPrefix: "mko_tkn_3KvL9X8z",
		TokenHash:   "hash-abc",
		ExpiresAt:   expires,
	})
	if err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}
	if rec.ID != "pat-1" || rec.Name != "cursor" {
		t.Errorf("Create: got %+v", rec)
	}
	if rec.RevokedAt != nil {
		t.Errorf("expected RevokedAt nil, got %v", rec.RevokedAt)
	}
	if !rec.ExpiresAt.Equal(expires) {
		t.Errorf("expected ExpiresAt=%v, got %v", expires, rec.ExpiresAt)
	}
	if len(rec.Scopes) != 1 || rec.Scopes[0] != "*" {
		t.Errorf("expected Scopes=[*], got %v", rec.Scopes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATRepo_Create_PersistsExplicitScopes(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now().UTC()
	expires := now.Add(7 * 24 * time.Hour)
	mock.ExpectQuery(`INSERT INTO meko_system\.mcp_personal_access_tokens`).
		WithArgs("user-1", "ci", "mko_tkn_aaaaaaaa", "hash-ci",
			sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(newMCPPATRow(
			"pat-ci", "user-1", "ci", "mko_tkn_aaaaaaaa",
			now, nil, expires, nil, scopesLit("read"),
		))

	repo := NewMCPPATRepo(db)
	//nolint:gosec // sqlmock test fixture, not a real credential.
	rec, err := repo.Create(context.Background(), MCPPATCreateParams{
		UserID:      "user-1",
		Name:        "ci",
		TokenPrefix: "mko_tkn_aaaaaaaa",
		TokenHash:   "hash-ci",
		ExpiresAt:   expires,
		Scopes:      []string{"read"},
	})
	if err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}
	if len(rec.Scopes) != 1 || rec.Scopes[0] != "read" {
		t.Errorf("expected Scopes=[read], got %v", rec.Scopes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATRepo_Create_NameConflictMapsToSentinel(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`INSERT INTO meko_system\.mcp_personal_access_tokens`).
		WithArgs("user-1", "cursor", "mko_tkn_3KvL9X8z", "hash-abc",
			sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(&pq.Error{Code: "23505", Constraint: "mcp_pats_token_hash_idx"})

	repo := NewMCPPATRepo(db)
	//nolint:gosec // sqlmock test fixture, not a real credential.
	_, err = repo.Create(context.Background(), MCPPATCreateParams{
		UserID:      "user-1",
		Name:        "cursor",
		TokenPrefix: "mko_tkn_3KvL9X8z",
		TokenHash:   "hash-abc",
		ExpiresAt:   time.Now().UTC().Add(time.Hour),
	})
	if !errors.Is(err, ErrMCPPATNameConflict) {
		t.Fatalf("expected ErrMCPPATNameConflict, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATRepo_ListByUser_ReturnsRowsNewestFirst(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now().UTC()
	older := now.Add(-time.Hour)
	expires := now.Add(30 * 24 * time.Hour)

	mock.ExpectQuery(`SELECT id, user_id, name, token_prefix,\s+created_at, last_used_at, expires_at, revoked_at, scopes\s+FROM meko_system\.mcp_personal_access_tokens\s+WHERE user_id = \$1`).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows(mcpPATColumns).
			AddRow("pat-2", "user-1", "claude", "mko_tkn_aaaaaaaa", now, nil, expires, nil, scopesLit("*")).
			AddRow("pat-1", "user-1", "cursor", "mko_tkn_bbbbbbbb", older, nil, expires, nil, scopesLit("read", "write")),
		)

	repo := NewMCPPATRepo(db)
	rows, err := repo.ListByUser(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].ID != "pat-2" || rows[1].ID != "pat-1" {
		t.Errorf("unexpected order: %+v", rows)
	}
	if len(rows[0].Scopes) != 1 || rows[0].Scopes[0] != "*" {
		t.Errorf("row[0]: expected Scopes=[*], got %v", rows[0].Scopes)
	}
	if len(rows[1].Scopes) != 2 || rows[1].Scopes[0] != "read" || rows[1].Scopes[1] != "write" {
		t.Errorf("row[1]: expected Scopes=[read write], got %v", rows[1].Scopes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATRepo_FindByHash_ReturnsRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	mock.ExpectQuery(`WHERE token_hash = \$1`).
		WithArgs("hash-abc").
		WillReturnRows(newMCPPATRow(
			"pat-1", "user-1", "cursor", "mko_tkn_3KvL9X8z",
			now, nil, expires, nil, scopesLit("*"),
		))

	repo := NewMCPPATRepo(db)
	got, err := repo.FindByHash(context.Background(), "hash-abc")
	if err != nil {
		t.Fatalf("FindByHash: %v", err)
	}
	if got == nil || got.ID != "pat-1" || got.UserID != "user-1" {
		t.Errorf("unexpected row: %+v", got)
	}
	if got != nil && (len(got.Scopes) != 1 || got.Scopes[0] != "*") {
		t.Errorf("expected Scopes=[*], got %v", got.Scopes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATRepo_FindByHash_ReturnsRevokedRowToo(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	mock.ExpectQuery(`WHERE token_hash = \$1`).
		WithArgs("hash-revoked").
		WillReturnRows(newMCPPATRow(
			"pat-r", "user-1", "cursor", "mko_tkn_3KvL9X8z",
			now, nil, expires, now, scopesLit("*"),
		))

	repo := NewMCPPATRepo(db)
	got, err := repo.FindByHash(context.Background(), "hash-revoked")
	if err != nil {
		t.Fatalf("FindByHash: %v", err)
	}
	if got == nil || got.RevokedAt == nil {
		t.Fatalf("expected revoked row to be returned, got %+v", got)
	}
	if got.IsActive(now) {
		t.Errorf("revoked row should not be active")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATRepo_FindByHash_NoRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`WHERE token_hash = \$1`).
		WithArgs("hash-missing").
		WillReturnRows(sqlmock.NewRows(mcpPATColumns))

	repo := NewMCPPATRepo(db)
	got, err := repo.FindByHash(context.Background(), "hash-missing")
	if err != nil {
		t.Fatalf("FindByHash: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATRepo_Revoke_UpdatesRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectExec(`UPDATE meko_system\.mcp_personal_access_tokens\s+SET revoked_at = NOW\(\)\s+WHERE user_id = \$1 AND id = \$2 AND revoked_at IS NULL`).
		WithArgs("user-1", "pat-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewMCPPATRepo(db)
	ok, err := repo.Revoke(context.Background(), "user-1", "pat-1")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if !ok {
		t.Errorf("expected ok=true, got false")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATRepo_Revoke_NoMatch(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectExec(`UPDATE meko_system\.mcp_personal_access_tokens`).
		WithArgs("user-1", "pat-1").
		WillReturnResult(sqlmock.NewResult(0, 0))

	repo := NewMCPPATRepo(db)
	ok, err := repo.Revoke(context.Background(), "user-1", "pat-1")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if ok {
		t.Errorf("expected ok=false, got true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATRepo_CountActiveByUser(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT COUNT\(\*\)\s+FROM meko_system\.mcp_personal_access_tokens`).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))

	repo := NewMCPPATRepo(db)
	n, err := repo.CountActiveByUser(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("CountActiveByUser: %v", err)
	}
	if n != 3 {
		t.Errorf("expected 3, got %d", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATRepo_NameExistsForUser(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`SELECT 1\s+FROM meko_system\.mcp_personal_access_tokens`).
		WithArgs("user-1", "cursor").
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))

	repo := NewMCPPATRepo(db)
	exists, err := repo.NameExistsForUser(context.Background(), "user-1", "cursor")
	if err != nil {
		t.Fatalf("NameExistsForUser: %v", err)
	}
	if !exists {
		t.Error("expected true")
	}

	mock.ExpectQuery(`SELECT 1\s+FROM meko_system\.mcp_personal_access_tokens`).
		WithArgs("user-1", "ghost").
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}))

	exists, err = repo.NameExistsForUser(context.Background(), "user-1", "ghost")
	if err != nil {
		t.Fatalf("NameExistsForUser ghost: %v", err)
	}
	if exists {
		t.Error("expected false")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATRepo_QueueLastUsedTouch_KeepsLatest(t *testing.T) {
	repo := NewMCPPATRepo(nil)

	t1 := time.Now().UTC()
	t2 := t1.Add(time.Minute)
	t3 := t1.Add(-time.Hour)

	repo.QueueLastUsedTouch("pat-1", t1)
	repo.QueueLastUsedTouch("pat-1", t2)
	repo.QueueLastUsedTouch("pat-1", t3) // should be ignored, older
	repo.QueueLastUsedTouch("", t1)      // empty id ignored

	if got := repo.PendingTouchCount(); got != 1 {
		t.Fatalf("PendingTouchCount = %d, want 1", got)
	}

	repo.touchMu.Lock()
	got := repo.touchPending["pat-1"]
	repo.touchMu.Unlock()
	if !got.Equal(t2) {
		t.Errorf("expected %v, got %v", t2, got)
	}
}

func TestMCPPATRepo_FlushLastUsedTouches_EmitsUpdates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	repo := NewMCPPATRepo(db)
	at := time.Now().UTC()
	repo.QueueLastUsedTouch("pat-1", at)
	repo.QueueLastUsedTouch("pat-2", at)

	mock.MatchExpectationsInOrder(false)
	mock.ExpectExec(`UPDATE meko_system\.mcp_personal_access_tokens\s+SET last_used_at`).
		WithArgs(at, "pat-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE meko_system\.mcp_personal_access_tokens\s+SET last_used_at`).
		WithArgs(at, "pat-2").
		WillReturnResult(sqlmock.NewResult(0, 1))

	n, err := repo.FlushLastUsedTouches(context.Background())
	if err != nil {
		t.Fatalf("FlushLastUsedTouches: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 rows updated, got %d", n)
	}
	if got := repo.PendingTouchCount(); got != 0 {
		t.Errorf("expected pending=0 after flush, got %d", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATRepo_FlushLastUsedTouches_Empty(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	repo := NewMCPPATRepo(db)
	n, err := repo.FlushLastUsedTouches(context.Background())
	if err != nil {
		t.Fatalf("FlushLastUsedTouches: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0, got %d", n)
	}
}
