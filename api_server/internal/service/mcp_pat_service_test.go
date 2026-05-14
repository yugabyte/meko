package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
)

func newPATService(t *testing.T) (
	*MCPPATService,
	sqlmock.Sqlmock,
	func(),
) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	patRepo := repository.NewMCPPATRepo(db)
	auditRepo := repository.NewMCPPATAuditRepo(db)
	svc := NewMCPPATService(patRepo, auditRepo)
	return svc, mock, func() {
		if err := db.Close(); err != nil {
			t.Logf("db.Close: %v", err)
		}
	}
}

// patFullRowCols matches the SELECT/RETURNING column ordering in
// mcp_pat_repo.go after the scopes column was added.
var patFullRowCols = []string{
	"id", "user_id", "name", "token_prefix",
	"created_at", "last_used_at", "expires_at", "revoked_at", "scopes",
}

// scopesLit returns a Postgres array literal usable as a sqlmock
// driver.Value for a TEXT[] column.
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

func TestMCPPATService_CreatePAT_HappyPath(t *testing.T) {
	svc, mock, cleanup := newPATService(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT COUNT\(\*\)`).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT 1\s+FROM meko_system\.mcp_personal_access_tokens`).
		WithArgs("user-1", "cursor").
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}))

	now := time.Now().UTC()
	expires := now.AddDate(0, 0, DefaultTTLDays)
	mock.ExpectQuery(`INSERT INTO meko_system\.mcp_personal_access_tokens`).
		WithArgs("user-1", "cursor", sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(patFullRowCols).AddRow(
			"pat-1", "user-1", "cursor", "mko_tkn_abcdef00",
			now, nil, expires, nil, scopesLit("*"),
		))

	mock.ExpectQuery(`INSERT INTO meko_system\.mcp_pat_audit_log`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("audit-1"))

	resp, err := svc.CreatePAT(context.Background(), MCPPATCreateInput{
		UserID:         "user-1",
		Name:           "  cursor  ",
		ActorIP:        "127.0.0.1",
		ActorUserAgent: "test/1",
	})
	if err != nil {
		t.Fatalf("CreatePAT: %v", err)
	}
	if !strings.HasPrefix(resp.Token, auth.PATScheme) {
		t.Errorf("expected token to start with %s, got %s", auth.PATScheme, resp.Token)
	}
	if resp.TokenRecord.ID != "pat-1" {
		t.Errorf("expected pat-1, got %s", resp.TokenRecord.ID)
	}
	if resp.TokenRecord.Name != "cursor" {
		t.Errorf("expected normalized name 'cursor', got %q", resp.TokenRecord.Name)
	}
	if len(resp.TokenRecord.Scopes) != 1 || resp.TokenRecord.Scopes[0] != "*" {
		t.Errorf("expected default Scopes=[*], got %v", resp.TokenRecord.Scopes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATService_CreatePAT_ExplicitScopes(t *testing.T) {
	svc, mock, cleanup := newPATService(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT COUNT\(\*\)`).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`SELECT 1\s+FROM meko_system\.mcp_personal_access_tokens`).
		WithArgs("user-1", "ci").
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}))

	now := time.Now().UTC()
	expires := now.AddDate(0, 0, DefaultTTLDays)
	mock.ExpectQuery(`INSERT INTO meko_system\.mcp_personal_access_tokens`).
		WithArgs("user-1", "ci", sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows(patFullRowCols).AddRow(
			"pat-ci", "user-1", "ci", "mko_tkn_aaaaaaaa",
			now, nil, expires, nil, scopesLit("read"),
		))
	mock.ExpectQuery(`INSERT INTO meko_system\.mcp_pat_audit_log`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("audit-1"))

	resp, err := svc.CreatePAT(context.Background(), MCPPATCreateInput{
		UserID: "user-1",
		Name:   "ci",
		Scopes: []string{"read"},
	})
	if err != nil {
		t.Fatalf("CreatePAT: %v", err)
	}
	if len(resp.TokenRecord.Scopes) != 1 || resp.TokenRecord.Scopes[0] != "read" {
		t.Errorf("expected Scopes=[read], got %v", resp.TokenRecord.Scopes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATService_CreatePAT_RejectsUnknownScope(t *testing.T) {
	svc, _, cleanup := newPATService(t)
	defer cleanup()

	_, err := svc.CreatePAT(context.Background(), MCPPATCreateInput{
		UserID: "user-1",
		Name:   "bad",
		Scopes: []string{"read", "admin"},
	})
	if !errors.Is(err, ErrMCPPATValidation) {
		t.Fatalf("expected ErrMCPPATValidation for unknown scope, got %v", err)
	}
}

func TestMCPPATService_CreatePAT_RejectsEmptyScopeEntry(t *testing.T) {
	svc, _, cleanup := newPATService(t)
	defer cleanup()

	_, err := svc.CreatePAT(context.Background(), MCPPATCreateInput{
		UserID: "user-1",
		Name:   "bad",
		Scopes: []string{"read", "  "},
	})
	if !errors.Is(err, ErrMCPPATValidation) {
		t.Fatalf("expected ErrMCPPATValidation for empty scope entry, got %v", err)
	}
}

func TestMCPPATService_CreatePAT_NameValidation(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"too_long", strings.Repeat("x", MaxPATNameLen+1)},
		{"control_char", "bad\x00name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, cleanup := newPATService(t)
			defer cleanup()

			_, err := svc.CreatePAT(context.Background(), MCPPATCreateInput{
				UserID: "user-1",
				Name:   tc.in,
			})
			if !errors.Is(err, ErrMCPPATValidation) {
				t.Fatalf("expected ErrMCPPATValidation, got %v", err)
			}
		})
	}
}

func TestMCPPATService_CreatePAT_CapReached(t *testing.T) {
	svc, mock, cleanup := newPATService(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT COUNT\(\*\)`).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(MaxActivePATsPerUser))

	_, err := svc.CreatePAT(context.Background(), MCPPATCreateInput{
		UserID: "user-1",
		Name:   "cursor",
	})
	if !errors.Is(err, ErrMCPPATCapReached) {
		t.Fatalf("expected ErrMCPPATCapReached, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATService_CreatePAT_NameInUse_PreCheck(t *testing.T) {
	svc, mock, cleanup := newPATService(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT COUNT\(\*\)`).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(`SELECT 1\s+FROM meko_system\.mcp_personal_access_tokens`).
		WithArgs("user-1", "cursor").
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))

	_, err := svc.CreatePAT(context.Background(), MCPPATCreateInput{
		UserID: "user-1",
		Name:   "cursor",
	})
	if !errors.Is(err, ErrMCPPATNameInUse) {
		t.Fatalf("expected ErrMCPPATNameInUse, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATService_RevokePAT_HappyPath(t *testing.T) {
	svc, mock, cleanup := newPATService(t)
	defer cleanup()

	now := time.Now().UTC()
	expires := now.Add(24 * time.Hour)
	mock.ExpectQuery(`WHERE user_id = \$1 AND id = \$2`).
		WithArgs("user-1", "pat-1").
		WillReturnRows(sqlmock.NewRows(patFullRowCols).AddRow(
			"pat-1", "user-1", "cursor", "mko_tkn_abcdef00",
			now, nil, expires, nil, scopesLit("*"),
		))

	mock.ExpectExec(`UPDATE meko_system\.mcp_personal_access_tokens\s+SET revoked_at`).
		WithArgs("user-1", "pat-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery(`INSERT INTO meko_system\.mcp_pat_audit_log`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("audit-1"))

	if err := svc.RevokePAT(context.Background(), MCPPATRevokeInput{
		UserID:  "user-1",
		TokenID: "pat-1",
	}); err != nil {
		t.Fatalf("RevokePAT: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATService_RevokePAT_NotFound(t *testing.T) {
	svc, mock, cleanup := newPATService(t)
	defer cleanup()

	mock.ExpectQuery(`WHERE user_id = \$1 AND id = \$2`).
		WithArgs("user-1", "pat-ghost").
		WillReturnRows(sqlmock.NewRows(patFullRowCols))

	err := svc.RevokePAT(context.Background(), MCPPATRevokeInput{
		UserID:  "user-1",
		TokenID: "pat-ghost",
	})
	if !errors.Is(err, ErrMCPPATNotFound) {
		t.Fatalf("expected ErrMCPPATNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATService_RevokePAT_AlreadyRevokedIsIdempotent(t *testing.T) {
	svc, mock, cleanup := newPATService(t)
	defer cleanup()

	now := time.Now().UTC()
	expires := now.Add(24 * time.Hour)
	mock.ExpectQuery(`WHERE user_id = \$1 AND id = \$2`).
		WithArgs("user-1", "pat-1").
		WillReturnRows(sqlmock.NewRows(patFullRowCols).AddRow(
			"pat-1", "user-1", "cursor", "mko_tkn_abcdef00",
			now, nil, expires, now, scopesLit("*"),
		))
	mock.ExpectExec(`UPDATE meko_system\.mcp_personal_access_tokens\s+SET revoked_at`).
		WithArgs("user-1", "pat-1").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := svc.RevokePAT(context.Background(), MCPPATRevokeInput{
		UserID:  "user-1",
		TokenID: "pat-1",
	}); err != nil {
		t.Fatalf("RevokePAT idempotent: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestClampTTLDays(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"zero defaults", 0, DefaultTTLDays},
		{"negative defaults", -5, DefaultTTLDays},
		{"in range", 30, 30},
		{"over max clamps down", MaxTTLDays + 100, MaxTTLDays},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampTTLDays(tc.in); got != tc.want {
				t.Errorf("clampTTLDays(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeName(t *testing.T) {
	got, err := normalizeName("  cursor  ")
	if err != nil {
		t.Fatalf("normalizeName: %v", err)
	}
	if got != "cursor" {
		t.Errorf("expected 'cursor', got %q", got)
	}
}

func TestNormalizeScopes(t *testing.T) {
	t.Run("empty defaults", func(t *testing.T) {
		got, err := normalizeScopes(nil)
		if err != nil {
			t.Fatalf("normalizeScopes(nil): %v", err)
		}
		if len(got) != 1 || got[0] != ScopeAll {
			t.Errorf("expected default %v, got %v", DefaultScopes, got)
		}
	})
	t.Run("preserves order and dedupes", func(t *testing.T) {
		got, err := normalizeScopes([]string{"read", "write", "read"})
		if err != nil {
			t.Fatalf("normalizeScopes: %v", err)
		}
		if len(got) != 2 || got[0] != "read" || got[1] != "write" {
			t.Errorf("expected [read write], got %v", got)
		}
	})
	t.Run("rejects unknown", func(t *testing.T) {
		_, err := normalizeScopes([]string{"admin"})
		if !errors.Is(err, ErrMCPPATValidation) {
			t.Fatalf("expected ErrMCPPATValidation, got %v", err)
		}
	})
	t.Run("rejects empty entry", func(t *testing.T) {
		_, err := normalizeScopes([]string{""})
		if !errors.Is(err, ErrMCPPATValidation) {
			t.Fatalf("expected ErrMCPPATValidation, got %v", err)
		}
	})
}

func TestMCPPATService_ValidatePAT_BadFormat(t *testing.T) {
	svc, _, cleanup := newPATService(t)
	defer cleanup()

	for _, in := range []string{"", "  ", "not-a-pat", "Bearer mko_tkn_xx"} {
		resp, err := svc.ValidatePAT(context.Background(), MCPPATValidateInput{Plaintext: in})
		if err != nil {
			t.Fatalf("ValidatePAT(%q): %v", in, err)
		}
		if resp.Valid || resp.Reason != models.MCPPATValidationUnknown {
			t.Errorf("ValidatePAT(%q): expected unknown invalid, got %+v", in, resp)
		}
	}
}

func TestMCPPATService_ValidatePAT_UnknownTokenNoAudit(t *testing.T) {
	svc, mock, cleanup := newPATService(t)
	defer cleanup()

	mock.ExpectQuery(`WHERE token_hash = \$1`).
		WillReturnRows(sqlmock.NewRows(patFullRowCols))

	plaintext, _, _, err := auth.GeneratePAT()
	if err != nil {
		t.Fatalf("GeneratePAT: %v", err)
	}
	resp, err := svc.ValidatePAT(context.Background(), MCPPATValidateInput{Plaintext: plaintext})
	if err != nil {
		t.Fatalf("ValidatePAT: %v", err)
	}
	if resp.Valid || resp.Reason != models.MCPPATValidationUnknown {
		t.Errorf("expected unknown invalid, got %+v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATService_ValidatePAT_ActiveTokenQueuesTouch(t *testing.T) {
	svc, mock, cleanup := newPATService(t)
	defer cleanup()

	now := time.Now().UTC()
	expires := now.Add(24 * time.Hour)
	mock.ExpectQuery(`WHERE token_hash = \$1`).
		WillReturnRows(sqlmock.NewRows(patFullRowCols).AddRow(
			"pat-1", "user-1", "cursor", "mko_tkn_abcdef00",
			now.Add(-time.Hour), nil, expires, nil, scopesLit("read", "write"),
		))

	plaintext, _, _, err := auth.GeneratePAT()
	if err != nil {
		t.Fatalf("GeneratePAT: %v", err)
	}

	resp, err := svc.ValidatePAT(context.Background(), MCPPATValidateInput{Plaintext: plaintext})
	if err != nil {
		t.Fatalf("ValidatePAT: %v", err)
	}
	if !resp.Valid {
		t.Fatalf("expected valid, got %+v", resp)
	}
	if resp.TokenID != "pat-1" || resp.UserID != "user-1" {
		t.Errorf("unexpected payload: %+v", resp)
	}
	if resp.ExpiresAt == nil || !resp.ExpiresAt.Equal(expires) {
		t.Errorf("expected ExpiresAt=%v, got %v", expires, resp.ExpiresAt)
	}
	if len(resp.Scopes) != 2 || resp.Scopes[0] != "read" || resp.Scopes[1] != "write" {
		t.Errorf("expected Scopes=[read write], got %v", resp.Scopes)
	}
	if got := svc.repo.PendingTouchCount(); got != 1 {
		t.Errorf("expected 1 queued touch, got %d", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATService_ValidatePAT_RevokedEmitsAudit(t *testing.T) {
	svc, mock, cleanup := newPATService(t)
	defer cleanup()

	now := time.Now().UTC()
	mock.ExpectQuery(`WHERE token_hash = \$1`).
		WillReturnRows(sqlmock.NewRows(patFullRowCols).AddRow(
			"pat-r", "user-1", "cursor", "mko_tkn_abcdef00",
			now.Add(-2*time.Hour), nil, now.Add(time.Hour),
			now.Add(-time.Minute), scopesLit("*"),
		))
	mock.ExpectQuery(`INSERT INTO meko_system\.mcp_pat_audit_log`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("audit-1"))

	plaintext, _, _, err := auth.GeneratePAT()
	if err != nil {
		t.Fatalf("GeneratePAT: %v", err)
	}
	resp, err := svc.ValidatePAT(context.Background(), MCPPATValidateInput{
		Plaintext: plaintext,
		ActorIP:   "10.0.0.1",
	})
	if err != nil {
		t.Fatalf("ValidatePAT: %v", err)
	}
	if resp.Valid || resp.Reason != models.MCPPATValidationRevoked {
		t.Errorf("expected revoked invalid, got %+v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATService_ValidatePAT_ExpiredEmitsAudit(t *testing.T) {
	svc, mock, cleanup := newPATService(t)
	defer cleanup()

	now := time.Now().UTC()
	mock.ExpectQuery(`WHERE token_hash = \$1`).
		WillReturnRows(sqlmock.NewRows(patFullRowCols).AddRow(
			"pat-e", "user-1", "cursor", "mko_tkn_abcdef00",
			now.Add(-30*24*time.Hour), nil, now.Add(-time.Minute), nil, scopesLit("*"),
		))
	mock.ExpectQuery(`INSERT INTO meko_system\.mcp_pat_audit_log`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("audit-1"))

	plaintext, _, _, err := auth.GeneratePAT()
	if err != nil {
		t.Fatalf("GeneratePAT: %v", err)
	}
	resp, err := svc.ValidatePAT(context.Background(), MCPPATValidateInput{Plaintext: plaintext})
	if err != nil {
		t.Fatalf("ValidatePAT: %v", err)
	}
	if resp.Valid || resp.Reason != models.MCPPATValidationExpired {
		t.Errorf("expected expired invalid, got %+v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestMCPPATService_ValidatePAT_RateLimitsRepeatAudits(t *testing.T) {
	svc, mock, cleanup := newPATService(t)
	defer cleanup()

	now := time.Now().UTC()
	revokedRow := func() *sqlmock.Rows {
		return sqlmock.NewRows(patFullRowCols).AddRow(
			"pat-r", "user-1", "cursor", "mko_tkn_abcdef00",
			now.Add(-2*time.Hour), nil, now.Add(time.Hour),
			now.Add(-time.Minute), scopesLit("*"),
		)
	}

	// 3 calls: only the first should produce an audit insert because
	// the rate limit window (5 min default) has not elapsed.
	mock.ExpectQuery(`WHERE token_hash = \$1`).WillReturnRows(revokedRow())
	mock.ExpectQuery(`INSERT INTO meko_system\.mcp_pat_audit_log`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("audit-1"))
	mock.ExpectQuery(`WHERE token_hash = \$1`).WillReturnRows(revokedRow())
	mock.ExpectQuery(`WHERE token_hash = \$1`).WillReturnRows(revokedRow())

	plaintext, _, _, err := auth.GeneratePAT()
	if err != nil {
		t.Fatalf("GeneratePAT: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := svc.ValidatePAT(context.Background(), MCPPATValidateInput{Plaintext: plaintext}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
