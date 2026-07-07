// Shared datapack-access gate used by every handler that mutates or
// reads tenant-scoped data via flat (non-`/datapacks/:name/...`) routes.
//
// Echo's `auth.RequireDatapackAccess` middleware only fits routes that
// carry `:name` in the path — for flat endpoints like POST
// /promote-memories, POST /keep-as-memory, DELETE /memories/:id, the
// datapack_id arrives in the body or as a query param, so the check
// has to run *inside* the handler. This file is the one place that
// check lives.

package handlers

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
)

// DatapackMembershipLookup verifies that a caller is currently a member
// of a datapack — either as the owner or via an accepted share. Both
// methods return (nil, nil) when no row matches; non-nil error is
// reserved for actual lookup failures.
type DatapackMembershipLookup interface {
	GetByID(userID, datapackID string) (*models.Datapack, error)
	GetByIDForSharedAccess(userID, datapackID string) (*models.Datapack, error)
}

// RequireDatapackAccess gates a handler endpoint on the caller currently
// being a member of `datapackID` (owner OR accepted share). On success,
// returns the caller's account ID. On any failure, writes the apiError
// response and returns the wrapping error so call sites can
// `return err` directly.
//
// Status mapping:
//   - empty / non-UUID datapack_id → 400 VALIDATION_ERROR
//   - missing auth context        → 401 UNAUTHORIZED
//   - datapack lookup errored     → 500 INTERNAL_ERROR
//   - not a member OR datapack doesn't exist → 404 NOT_FOUND
//
// The "not a member" and "doesn't exist" cases deliberately collapse
// into one 404 — same convention as `auth.RequireDatapackAccess`
// middleware, so a caller can't probe membership by status code.
func RequireDatapackAccess(
	c echo.Context,
	dpLookup DatapackMembershipLookup,
	datapackID string,
) (accountID string, err error) {
	_, accountID, err = resolveDatapackMembership(c, dpLookup, datapackID)
	return accountID, err
}

// RequireDatapackOwnerOrMaintainer is the stricter sibling of
// RequireDatapackAccess: same membership lookup, but callers whose resolved
// grant is viewer or contributor are rejected with 403. It's the handler-level
// equivalent of auth.RequireDatapackOwnerOrMaintainerGrant middleware, which
// only fits routes that carry :datapack_id in the path (promote-memories is a
// flat route with datapack_id in the body). GetByID resolves the owner to
// grant "owner"; GetByIDForSharedAccess resolves shared grants from the
// accepted invite.
func RequireDatapackOwnerOrMaintainer(
	c echo.Context,
	dpLookup DatapackMembershipLookup,
	datapackID string,
) (accountID string, err error) {
	dp, accountID, err := resolveDatapackMembership(c, dpLookup, datapackID)
	if err != nil {
		return "", err
	}
	if !models.IsOwnerOrMaintainerGrant(dp.Grant) {
		return "", apiError(c, http.StatusForbidden, "FORBIDDEN",
			"only datapack owners and maintainers are allowed")
	}
	return accountID, nil
}

// resolveDatapackMembership runs the shared owner-then-shared lookup and status
// mapping for the two guards above. On success returns the resolved datapack
// (with Grant populated) and the caller's account ID.
func resolveDatapackMembership(
	c echo.Context,
	dpLookup DatapackMembershipLookup,
	datapackID string,
) (dp *models.Datapack, accountID string, err error) {
	datapackID = strings.TrimSpace(datapackID)
	if datapackID == "" {
		return nil, "", apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", "datapack_id is required")
	}
	if _, parseErr := uuid.Parse(datapackID); parseErr != nil {
		return nil, "", apiError(c, http.StatusBadRequest, "VALIDATION_ERROR", "datapack_id must be a valid UUID")
	}
	account, authErr := auth.GetAccount(c)
	if authErr != nil || account == nil {
		return nil, "", apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing account context")
	}
	dp, dpErr := dpLookup.GetByID(account.ID, datapackID)
	if dpErr != nil {
		return nil, "", apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", dpErr.Error())
	}
	if dp == nil {
		dp, dpErr = dpLookup.GetByIDForSharedAccess(account.ID, datapackID)
		if dpErr != nil {
			return nil, "", apiError(c, http.StatusInternalServerError, "INTERNAL_ERROR", dpErr.Error())
		}
	}
	if dp == nil {
		return nil, "", apiError(c, http.StatusNotFound, "NOT_FOUND", "datapack not found")
	}
	return dp, account.ID, nil
}
