package handlers_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/handlers"
	"meko/api_server/internal/models"
	"meko/api_server/internal/service"
)

// authedPostJSON builds an authed POST context carrying a JSON body, so a
// handler's Bind and guards run as they would in the server.
func authedPostJSON(accountID, body string) (echo.Context, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.Set(auth.ContextKeyAccount, &models.Account{ID: accountID})
	return c, rec
}

// A non-UUID memory id must be refused with 400 VALIDATION_ERROR before the mem0
// fetch, never surfaced as a 500 carrying the Postgres cast error (MEKO-834).
func TestPromote_NonUUIDMemoryIDIsRefusedCleanly(t *testing.T) {
	const acct, dp = "acct-1", "11111111-1111-1111-1111-111111111111"
	lookup := &fakeDatapackLookup{owned: map[string]*models.Datapack{
		acct + ":" + dp: {ID: dp, Grant: models.GrantOwner},
	}}
	h := handlers.NewPromoteMemoriesHandler(service.NewPromoteMemoriesService(nil), lookup)

	c, rec := authedPostJSON(acct, `{"datapack_id":"`+dp+`","memory_ids":["not-a-uuid"]}`)
	if err := h.Promote(c); err != nil {
		t.Fatalf("Promote returned a raw error: %v", err)
	}
	assertCleanBadRequest(t, rec, "not-a-uuid")
}

// uuid.Parse accepts urn:uuid:/{braces}/hyphenless forms the Postgres uuid cast
// can still reject, so the guard must refuse them too, not just obvious garbage —
// otherwise they reach SQL and reproduce the 500 (MEKO-834 review).
func TestPromote_LenientUUIDFormsAreRefused(t *testing.T) {
	const acct, dp = "acct-3", "33333333-3333-3333-3333-333333333333"
	lookup := &fakeDatapackLookup{owned: map[string]*models.Datapack{
		acct + ":" + dp: {ID: dp, Grant: models.GrantOwner},
	}}
	h := handlers.NewPromoteMemoriesHandler(service.NewPromoteMemoriesService(nil), lookup)

	for _, bad := range []string{
		"urn:uuid:11111111-1111-1111-1111-111111111111",
		"{11111111-1111-1111-1111-111111111111}",
		"11111111111111111111111111111111",
	} {
		c, rec := authedPostJSON(acct, `{"datapack_id":"`+dp+`","memory_ids":["`+bad+`"]}`)
		if err := h.Promote(c); err != nil {
			t.Fatalf("Promote(%q) returned a raw error: %v", bad, err)
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("Promote(%q): status = %d, want 400; body=%s", bad, rec.Code, rec.Body.String())
		}
	}
}

// KeepAsMemory shares the same unchecked-id path (MarkAsKept), so it is guarded too.
func TestKeepAsMemory_NonUUIDMemoryIDIsRefusedCleanly(t *testing.T) {
	const acct, dp = "acct-2", "22222222-2222-2222-2222-222222222222"
	lookup := &fakeDatapackLookup{owned: map[string]*models.Datapack{
		acct + ":" + dp: {ID: dp, Grant: string(models.GrantViewer)},
	}}
	h := handlers.NewPromoteMemoriesHandler(service.NewPromoteMemoriesService(nil), lookup)

	c, rec := authedPostJSON(acct, `{"datapack_id":"`+dp+`","memory_ids":["bogus"]}`)
	if err := h.KeepAsMemory(c); err != nil {
		t.Fatalf("KeepAsMemory returned a raw error: %v", err)
	}
	assertCleanBadRequest(t, rec, "bogus")
}

func assertCleanBadRequest(t *testing.T, rec *httptest.ResponseRecorder, badID string) {
	t.Helper()
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "VALIDATION_ERROR") {
		t.Errorf("want VALIDATION_ERROR code, got %s", body)
	}
	if !strings.Contains(body, badID) {
		t.Errorf("response should name the bad id %q, got %s", badID, body)
	}
	if strings.Contains(body, "pq:") || strings.Contains(strings.ToLower(body), "syntax for type uuid") {
		t.Errorf("response leaked database error text: %s", body)
	}
}
