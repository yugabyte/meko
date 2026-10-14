package handlers_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/handlers"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// A redeem that keeps losing to concurrent joins is 503 with Retry-After, so the
// UI can say "try again" instead of showing a generic error.
func TestRedeem_BusyLinkIs503(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)
	for range 4 {
		mock.ExpectBegin()
		mock.ExpectExec("FOR UPDATE").WillReturnError(&pq.Error{Code: "40001", Message: "could not serialize access"})
		mock.ExpectRollback()
	}
	h := handlers.NewShareLinkHandler(service.NewShareLinkService(repository.NewShareLinkRepo(db), nil))

	rec := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/", nil), rec)
	c.Set(auth.ContextKeyAccount, &models.Account{ID: "u1", Email: "u1@x.com"})
	c.SetParamNames("token")
	c.SetParamValues("tok")

	if err := h.Redeem(c); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Errorf("got %d (Retry-After %q): %s; want 503 with Retry-After",
			rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
}
