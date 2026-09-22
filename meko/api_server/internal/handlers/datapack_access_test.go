package handlers_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/handlers"
	"meko/api_server/internal/models"
)

// fakeDatapackLookup is a minimal handlers.DatapackMembershipLookup, keyed
// by "userID:datapackID" so a test can grant exactly the pairs it names.
type fakeDatapackLookup struct {
	owned  map[string]*models.Datapack
	shared map[string]*models.Datapack
}

func (f *fakeDatapackLookup) GetByID(userID, datapackID string) (*models.Datapack, error) {
	return f.owned[userID+":"+datapackID], nil
}

func (f *fakeDatapackLookup) GetByIDForSharedAccess(userID, datapackID string) (*models.Datapack, error) {
	return f.shared[userID+":"+datapackID], nil
}

func newAuthedContext(accountID string) (echo.Context, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.Set(auth.ContextKeyAccount, &models.Account{ID: accountID})
	return c, rec
}

const otherTenantsDatapackID = "4f9b4181-83ce-49e0-9fce-e5541081adb0"

// TestRequireDatapackAccess_DeniedReturnsRealError is the direct regression
// test for MEKO-656 / MEKO-690: supplying a datapack_id the caller has no
// grant on must yield a non-nil error a caller's own
// `if err != nil { return err }` can act on -- not just a written response
// body. Before the fix this returned (accountID, nil) even on denial
// (apiError's return is c.JSON's write error, nil on any successful write
// regardless of status), which is why MemoriesHandler.ListGET and friends
// fell through into the real query using the unverified datapack_id.
func TestRequireDatapackAccess_DeniedReturnsRealError(t *testing.T) {
	c, rec := newAuthedContext("attacker")
	lookup := &fakeDatapackLookup{} // no grants for anyone

	accountID, err := handlers.RequireDatapackAccess(c, lookup, otherTenantsDatapackID)

	if err == nil {
		t.Fatal("expected a non-nil error when the caller has no grant on the datapack")
	}
	if accountID != "" {
		t.Errorf("expected empty accountID on denial, got %q", accountID)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 written to the response, got %d", rec.Code)
	}
}

func TestRequireDatapackAccess_OwnedGrantsAccess(t *testing.T) {
	c, _ := newAuthedContext("acct-1")
	lookup := &fakeDatapackLookup{
		owned: map[string]*models.Datapack{
			"acct-1:" + otherTenantsDatapackID: {ID: otherTenantsDatapackID, Grant: models.GrantOwner},
		},
	}

	accountID, err := handlers.RequireDatapackAccess(c, lookup, otherTenantsDatapackID)

	if err != nil {
		t.Fatalf("expected nil error for an owned datapack, got %v", err)
	}
	if accountID != "acct-1" {
		t.Errorf("expected accountID acct-1, got %q", accountID)
	}
}

func TestRequireDatapackAccess_SharedGrantsAccess(t *testing.T) {
	c, _ := newAuthedContext("acct-2")
	lookup := &fakeDatapackLookup{
		shared: map[string]*models.Datapack{
			"acct-2:" + otherTenantsDatapackID: {ID: otherTenantsDatapackID, Grant: string(models.GrantContributor)},
		},
	}

	accountID, err := handlers.RequireDatapackAccess(c, lookup, otherTenantsDatapackID)

	if err != nil {
		t.Fatalf("expected nil error for a shared datapack, got %v", err)
	}
	if accountID != "acct-2" {
		t.Errorf("expected accountID acct-2, got %q", accountID)
	}
}

// TestRequireDatapackOwnerOrMaintainer_ContributorDeniedReturnsRealError
// covers the write-scoped sibling (promote-memories, keep-as-memory): a
// contributor grant is real membership but not enough for these routes, and
// that denial must also propagate as a real error, not just a 403 body.
func TestRequireDatapackOwnerOrMaintainer_ContributorDeniedReturnsRealError(t *testing.T) {
	c, rec := newAuthedContext("acct-3")
	lookup := &fakeDatapackLookup{
		shared: map[string]*models.Datapack{
			"acct-3:" + otherTenantsDatapackID: {ID: otherTenantsDatapackID, Grant: string(models.GrantContributor)},
		},
	}

	accountID, err := handlers.RequireDatapackOwnerOrMaintainer(c, lookup, otherTenantsDatapackID)

	if err == nil {
		t.Fatal("expected a non-nil error for a contributor-only grant")
	}
	if accountID != "" {
		t.Errorf("expected empty accountID on denial, got %q", accountID)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 written to the response, got %d", rec.Code)
	}
}

func TestRequireDatapackOwnerOrMaintainer_OwnerGrantsAccess(t *testing.T) {
	c, _ := newAuthedContext("acct-4")
	lookup := &fakeDatapackLookup{
		owned: map[string]*models.Datapack{
			"acct-4:" + otherTenantsDatapackID: {ID: otherTenantsDatapackID, Grant: models.GrantOwner},
		},
	}

	accountID, err := handlers.RequireDatapackOwnerOrMaintainer(c, lookup, otherTenantsDatapackID)

	if err != nil {
		t.Fatalf("expected nil error for an owner grant, got %v", err)
	}
	if accountID != "acct-4" {
		t.Errorf("expected accountID acct-4, got %q", accountID)
	}
}
