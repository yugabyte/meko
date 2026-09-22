package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/handlers"
	"meko/api_server/internal/models"
)

// TestMemoriesHandler_ListGET_DeniedDatapackWritesExactlyOneResponse is the
// direct regression test for the HackerOne report behind MEKO-656 / MEKO-690:
// GET /memories with a datapack_id the caller has no grant on returned a 404
// body followed by a second, real JSON body containing another tenant's
// memories concatenated into the same response. Before the fix,
// RequireDatapackAccess's denial returned a nil error (apiError's return is
// c.JSON's write error, always nil on a successful write), so ListGET fell
// through past its own early-return check and ran the real query.
//
// memoriesSvc/langfuseSvc are left nil: with the fix, denial must return
// before either is ever touched, so a nil pointer here is itself proof the
// handler stopped -- touching them would panic instead of asserting.
func TestMemoriesHandler_ListGET_DeniedDatapackWritesExactlyOneResponse(t *testing.T) {
	lookup := &fakeDatapackLookup{} // no grants for anyone
	handler := handlers.NewMemoriesHandler(nil, nil, lookup)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/memories?datapack_id="+otherTenantsDatapackID, nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.Set(auth.ContextKeyAccount, &models.Account{ID: "attacker"})

	err := handler.ListGET(c)

	if err == nil {
		t.Fatal("expected ListGET to return a non-nil error for a denied datapack_id")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	dec := json.NewDecoder(rec.Body)
	var body handlers.APIResponse
	if decodeErr := dec.Decode(&body); decodeErr != nil {
		t.Fatalf("failed to decode response body: %v", decodeErr)
	}
	if body.Error == nil || body.Error.Code != "NOT_FOUND" {
		t.Fatalf("expected a NOT_FOUND error envelope, got %+v", body)
	}
	if body.Data != nil {
		t.Fatalf("expected the error response to carry no data, got %+v", body.Data)
	}
	// The actual vulnerability: a second JSON document concatenated after
	// the first in the same response body. dec.More() reports whether the
	// stream has additional data after the value just decoded.
	if dec.More() {
		var leaked json.RawMessage
		_ = dec.Decode(&leaked)
		t.Fatalf("response body contains a second JSON document after the error envelope: %s", leaked)
	}
}
