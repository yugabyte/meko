package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// ampHandlerWithNoPools builds an AMPHandler whose owner owns zero AMP pools, so
// SumMilliCoresByOwner resolves to 0 milli-cores without any controller
// fan-out — enough to exercise the tier gate + budget math end to end. Returns
// the handler and the sqlmock so a caller can add expectations first.
func ampHandlerWithNoPools(t *testing.T) (*AMPHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	svc := service.NewAMPService(repository.NewAMPRepo(db), repository.NewAMPRegistrationRepo(db))
	return NewAMPHandler(svc), mock, func() { _ = db.Close() }
}

func ctxWithAccount(userType string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	c.Set(auth.ContextKeyAccount, &models.Account{ID: "u1", AccountType: userType})
	return c, rec
}

// expectNoPools queues the ListByOwner read as returning zero rows.
func expectNoPools(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`FROM   meko_system.amp_datapack_pool`).
		WithArgs("u1").
		WillReturnRows(sqlmock.NewRows(
			[]string{"datapack_id", "shared_pool_id", "owner_user_id", "status", "created_at", "updated_at"}))
}

// enforceCoreLimit returns a 402 when the reservation would exceed the tier cap,
// nil (proceed) when it fits, and skips the fan-out entirely for unlimited tiers.
func TestEnforceCoreLimit(t *testing.T) {
	cases := []struct {
		name       string
		userType   string
		addMilli   int64
		wantStatus int    // 0 = nil (proceed)
		wantCode   string // 402 code when blocked
		hasPools   bool   // whether ListByOwner is consulted
	}{
		{"free over the 0.5-core cap", "free", 2000, http.StatusPaymentRequired, service.CodeFreeTierCoreLimit, true},
		{"free exactly at the cap proceeds", "free", 500, 0, "", true},
		{"enterprise (unlimited) skips the fan-out", "enterprise", 8000, 0, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, mock, done := ampHandlerWithNoPools(t)
			defer done()
			if tc.hasPools {
				expectNoPools(mock)
			}
			c, rec := ctxWithAccount(tc.userType)

			err := h.enforceCoreLimit(c, tc.userType, "u1", tc.addMilli)
			if tc.wantStatus == 0 {
				if err != nil {
					t.Fatalf("want proceed (nil), got %v", err)
				}
			} else {
				if rec.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
				}
				var resp APIResponse
				_ = json.Unmarshal(rec.Body.Bytes(), &resp)
				if resp.Error == nil || resp.Error.Code != tc.wantCode {
					t.Fatalf("code = %+v, want %s", resp.Error, tc.wantCode)
				}
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet expectations: %v", err)
			}
		})
	}
}

// CoreBudget reports the tier cap + current usage; an unlimited tier returns a
// null max with no fan-out, and it degrades to not-at-limit (never 5xx) when the
// usage read fails.
func TestCoreBudget(t *testing.T) {
	t.Run("free, no pools -> 0.5 cap, not at limit", func(t *testing.T) {
		h, mock, done := ampHandlerWithNoPools(t)
		defer done()
		expectNoPools(mock)
		c, rec := ctxWithAccount("free")

		if err := h.CoreBudget(c); err != nil {
			t.Fatalf("CoreBudget: %v", err)
		}
		var resp struct {
			Data coreBudgetResponse `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp.Data.MaxCores == nil || *resp.Data.MaxCores != 0.5 {
			t.Errorf("max_cores = %v, want 0.5", resp.Data.MaxCores)
		}
		if resp.Data.UsedCores != 0 || resp.Data.AtLimit || resp.Data.NextTier != "standard" {
			t.Errorf("unexpected budget: %+v", resp.Data)
		}
	})

	t.Run("enterprise -> null cap, no fan-out", func(t *testing.T) {
		h, _, done := ampHandlerWithNoPools(t)
		defer done()
		c, rec := ctxWithAccount("enterprise") // no ListByOwner expectation: must not query

		if err := h.CoreBudget(c); err != nil {
			t.Fatalf("CoreBudget: %v", err)
		}
		var resp struct {
			Data coreBudgetResponse `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp.Data.MaxCores != nil || resp.Data.AtLimit {
			t.Errorf("enterprise should be uncapped: %+v", resp.Data)
		}
	})

	t.Run("degrades to not-at-limit when usage read fails", func(t *testing.T) {
		h, mock, done := ampHandlerWithNoPools(t)
		defer done()
		// One pool exists, but resolving the controller registration finds none
		// -> SumMilliCoresByOwner errors -> CoreBudget must still answer 200.
		mock.ExpectQuery(`FROM   meko_system.amp_datapack_pool`).
			WithArgs("u1").
			WillReturnRows(sqlmock.NewRows(
				[]string{"datapack_id", "shared_pool_id", "owner_user_id", "status", "created_at", "updated_at"}).
				AddRow("dp1", "p", "u1", "ready", time.Time{}, time.Time{}))
		mock.ExpectQuery(`FROM   meko_system.amp_registration`).
			WillReturnError(context.DeadlineExceeded)
		c, rec := ctxWithAccount("free")

		if err := h.CoreBudget(c); err != nil {
			t.Fatalf("CoreBudget: %v", err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (degrade)", rec.Code)
		}
		var resp struct {
			Data coreBudgetResponse `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp.Data.AtLimit || resp.Data.UsedCores != 0 {
			t.Errorf("want degrade to used=0/not-at-limit, got %+v", resp.Data)
		}
	})
}

// ampError must map the not-registered sentinel to its own status/code —
// MCP tools and the inference gateway branch on AMP_NOT_REGISTERED to tell
// users "an admin must register AMP first" instead of a generic AMP failure.
func TestAMPErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"not registered", service.ErrAMPNotRegistered,
			http.StatusPreconditionFailed, "AMP_NOT_REGISTERED"},
		{"wrapped not registered", errors.Join(errors.New("ctx"), service.ErrAMPNotRegistered),
			http.StatusPreconditionFailed, "AMP_NOT_REGISTERED"},
		{"controller failure", errors.New("connection refused"),
			http.StatusBadGateway, "AMP_ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			rec := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)

			if err := ampError(c, tc.err); err != nil {
				t.Fatalf("ampError returned error: %v", err)
			}
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			var resp APIResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if resp.Error == nil || resp.Error.Code != tc.wantCode {
				t.Fatalf("error code = %+v, want %s", resp.Error, tc.wantCode)
			}
		})
	}
}
