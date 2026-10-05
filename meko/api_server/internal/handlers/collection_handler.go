package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// CollectionHandler holds Echo handlers for the collection tree: collections, their
// folders, and where datapacks sit inside them.
type CollectionHandler struct {
	svc *service.CollectionService
}

// NewCollectionHandler creates a new CollectionHandler.
func NewCollectionHandler(svc *service.CollectionService) *CollectionHandler {
	return &CollectionHandler{svc: svc}
}

// collectionStatus maps a service/repository error to its HTTP answer.
//
// One place rather than a repeated switch per handler: every collection endpoint
// can produce the same four outcomes, and a name collision in particular has
// to be a 409 rather than a 500 so the UI can prompt for a different name.
func collectionStatus(err error) (int, string) {
	switch {
	case errors.Is(err, service.ErrInvalidCollectionName),
		errors.Is(err, service.ErrUnknownTheme):
		return http.StatusBadRequest, "VALIDATION_ERROR"
	case errors.Is(err, repository.ErrCollectionNotFound),
		errors.Is(err, repository.ErrFolderNotFound),
		errors.Is(err, service.ErrFolderNotInCollection):
		return http.StatusNotFound, "NOT_FOUND"
	case errors.Is(err, service.ErrNotDatapackOwner):
		return http.StatusForbidden, "FORBIDDEN"
	case errors.Is(err, repository.ErrCollectionNameTaken),
		errors.Is(err, repository.ErrFolderNameTaken):
		return http.StatusConflict, "CONFLICT"
	default:
		return http.StatusInternalServerError, "INTERNAL_ERROR"
	}
}

func (h *CollectionHandler) fail(c echo.Context, err error) error {
	status, code := collectionStatus(err)
	return apiError(c, status, code, err.Error())
}

// accountID resolves the caller. Every collection route is JWT-only and
// user-scoped, so this guard is the same on all of them.
func accountID(c echo.Context) (string, error) {
	account, err := auth.GetAccount(c)
	if err != nil || account == nil {
		return "", errors.New("missing account context")
	}
	return account.ID, nil
}

// Themes handles GET /api/v1/collections/themes.
//
// Served from Go constants, not a table: a theme is stamped into a collection's
// folders at creation and never read again. The UI renders the picker from
// this so there is one source of truth for the layouts.
//
//	@Summary	List collection themes
//	@Tags		collections
//	@Produce	json
//	@Success	200	{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/collections/themes [get]
func (h *CollectionHandler) Themes(c echo.Context) error {
	return ok(c, http.StatusOK, service.CollectionThemes())
}

// List handles GET /api/v1/collections.
//
//	@Summary	List collections with folder and datapack counts
//	@Tags		collections
//	@Produce	json
//	@Success	200	{object}	APIResponse
//	@Failure	401	{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/collections [get]
func (h *CollectionHandler) List(c echo.Context) error {
	userID, err := accountID(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
	}
	owned, err := h.svc.ListWithCounts(userID)
	if err != nil {
		return h.fail(c, err)
	}
	shared, err := h.svc.ListSharedWithCounts(userID)
	if err != nil {
		return h.fail(c, err)
	}
	// Two lists rather than one flagged list: they render as separate sections
	// and a recipient can do far less with a shared collection, so keeping them
	// apart on the wire stops a client treating them alike by accident.
	return ok(c, http.StatusOK, collectionListResponse{Owned: owned, Shared: shared})
}

// collectionListResponse is the body of GET /collections.
type collectionListResponse struct {
	Owned  []models.Collection `json:"owned"`
	Shared []models.Collection `json:"shared"`
}

// Page size for the collection activity feed. Shared with the per-datapack
// endpoint's own defaults so "Show more" behaves the same on both.
const (
	activityLogDefaultLimit = 10
	activityLogMaxLimit     = 50
)

// ActivityLog handles GET /api/v1/collections/:collection_id/activity-log.
//
//	@Summary	Uploads, promotions, folder creations and filings across a collection, newest first
//	@Tags		collections
//	@Produce	json
//	@Param		collection_id	path		string	true	"Collection ID"
//	@Param		limit		query		int		false	"Page size (default 10, max 50)"
//	@Param		cursor		query		string	false	"Opaque cursor from the previous page"
//	@Success	200			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/collections/{collection_id}/activity-log [get]
func (h *CollectionHandler) ActivityLog(c echo.Context) error {
	userID, err := accountID(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
	}

	// Capped like the per-datapack feed: both sides read every row in scope
	// and sort, so an uncapped limit would be an uncapped read.
	limit := activityLogDefaultLimit
	if v := strings.TrimSpace(c.QueryParam("limit")); v != "" {
		if n, pErr := strconv.Atoi(v); pErr == nil && n > 0 {
			if n > activityLogMaxLimit {
				n = activityLogMaxLimit
			}
			limit = n
		}
	}

	resp, err := h.svc.ActivityLog(
		userID, c.Param("collection_id"), limit, strings.TrimSpace(c.QueryParam("cursor")),
	)
	if err != nil {
		return h.fail(c, err)
	}
	return ok(c, http.StatusOK, resp)
}

// Create handles POST /api/v1/collections.
//
//	@Summary	Create a collection from a theme
//	@Tags		collections
//	@Accept		json
//	@Produce	json
//	@Param		body	body		service.CreateCollectionRequest	true	"Create request"
//	@Success	201		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	409		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/collections [post]
func (h *CollectionHandler) Create(c echo.Context) error {
	userID, err := accountID(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
	}
	var req service.CreateCollectionRequest
	if bErr := c.Bind(&req); bErr != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	collection, err := h.svc.Create(userID, req)
	if err != nil {
		return h.fail(c, err)
	}
	return ok(c, http.StatusCreated, collection)
}

// Detail handles GET /api/v1/collections/:collection_id — the collection and its
// folder tree, each folder carrying its own datapack count.
//
//	@Summary	Get a collection, its folders, and the datapacks in them with counts
//	@Tags		collections
//	@Produce	json
//	@Param		collection_id	path		string	true	"Collection ID"
//	@Success	200			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/collections/{collection_id} [get]
func (h *CollectionHandler) Detail(c echo.Context) error {
	userID, err := accountID(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
	}
	detail, err := h.svc.Detail(userID, c.Param("collection_id"))
	if err != nil {
		return h.fail(c, err)
	}
	return ok(c, http.StatusOK, detail)
}

// updateCollectionRequest is the body of PATCH /collections/:collection_id.
type updateCollectionRequest struct {
	Name        string `json:"collection_name"`
	Description string `json:"description"`
}

// Update handles PATCH /api/v1/collections/:collection_id.
//
//	@Summary	Rename a collection
//	@Tags		collections
//	@Accept		json
//	@Produce	json
//	@Param		collection_id	path		string	true	"Collection ID"
//	@Success	200			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Failure	409			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/collections/{collection_id} [patch]
func (h *CollectionHandler) Update(c echo.Context) error {
	userID, err := accountID(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
	}
	var req updateCollectionRequest
	if bErr := c.Bind(&req); bErr != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	if err = h.svc.Rename(userID, c.Param("collection_id"), req.Name, req.Description); err != nil {
		return h.fail(c, err)
	}
	return ok(c, http.StatusOK, map[string]string{"collection_id": c.Param("collection_id")})
}

// Delete handles DELETE /api/v1/collections/:collection_id.
//
// The datapacks inside are NOT deleted — they become unfiled. That is a
// property of the schema (placements cascade, datapacks don't), and it is what
// makes deleting a collection recoverable by refiling.
//
//	@Summary	Delete a collection
//	@Tags		collections
//	@Produce	json
//	@Param		collection_id	path		string	true	"Collection ID"
//	@Success	200			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/collections/{collection_id} [delete]
func (h *CollectionHandler) Delete(c echo.Context) error {
	userID, err := accountID(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
	}
	if err = h.svc.Delete(userID, c.Param("collection_id")); err != nil {
		return h.fail(c, err)
	}
	return ok(c, http.StatusOK, map[string]string{"collection_id": c.Param("collection_id")})
}

// createFolderRequest is the body of POST /collections/:collection_id/folders.
type createFolderRequest struct {
	Name string `json:"folder_name"`
}

// CreateFolder handles POST /api/v1/collections/:collection_id/folders.
//
//	@Summary	Add a folder to a collection
//	@Tags		collections
//	@Accept		json
//	@Produce	json
//	@Param		collection_id	path		string	true	"Collection ID"
//	@Success	201			{object}	APIResponse
//	@Failure	400			{object}	APIResponse
//	@Failure	409			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/collections/{collection_id}/folders [post]
func (h *CollectionHandler) CreateFolder(c echo.Context) error {
	userID, err := accountID(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
	}
	var req createFolderRequest
	if bErr := c.Bind(&req); bErr != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	folder, err := h.svc.CreateFolder(userID, c.Param("collection_id"), req.Name)
	if err != nil {
		return h.fail(c, err)
	}
	return ok(c, http.StatusCreated, folder)
}

// updateFolderRequest is the body of PATCH
// /collections/:collection_id/folders/:folder_id.
type updateFolderRequest struct {
	Name string `json:"folder_name"`
}

// UpdateFolder handles PATCH /api/v1/collections/:collection_id/folders/:folder_id.
//
//	@Summary	Rename a folder
//	@Tags		collections
//	@Accept		json
//	@Produce	json
//	@Param		collection_id	path		string	true	"Collection ID"
//	@Param		folder_id	path		string	true	"Folder ID"
//	@Success	200			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Failure	409			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/collections/{collection_id}/folders/{folder_id} [patch]
func (h *CollectionHandler) UpdateFolder(c echo.Context) error {
	userID, err := accountID(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
	}
	var req updateFolderRequest
	if bErr := c.Bind(&req); bErr != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	if err = h.svc.RenameFolder(userID, c.Param("collection_id"), c.Param("folder_id"), req.Name); err != nil {
		return h.fail(c, err)
	}
	return ok(c, http.StatusOK, map[string]string{"folder_id": c.Param("folder_id")})
}

// DeleteFolder handles DELETE
// /api/v1/collections/:collection_id/folders/:folder_id.
//
//	@Summary	Delete a folder and its subfolders
//	@Tags		collections
//	@Produce	json
//	@Param		collection_id	path		string	true	"Collection ID"
//	@Param		folder_id	path		string	true	"Folder ID"
//	@Success	200			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/collections/{collection_id}/folders/{folder_id} [delete]
func (h *CollectionHandler) DeleteFolder(c echo.Context) error {
	userID, err := accountID(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
	}
	if err = h.svc.DeleteFolder(userID, c.Param("collection_id"), c.Param("folder_id")); err != nil {
		return h.fail(c, err)
	}
	return ok(c, http.StatusOK, map[string]string{"folder_id": c.Param("folder_id")})
}

// DatapackPaths handles GET /api/v1/collections/placements — where every filed
// datapack sits in the caller's tree, keyed by datapack_id.
//
// One call labels the whole Datapacks page. A datapack missing from the map is
// unfiled, so the client needs no second request to tell the two apart.
//
//	@Summary	Map every filed datapack to its path
//	@Tags		collections
//	@Produce	json
//	@Success	200	{object}	APIResponse
//	@Failure	401	{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/collections/placements [get]
func (h *CollectionHandler) DatapackPaths(c echo.Context) error {
	userID, err := accountID(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
	}
	paths, err := h.svc.DatapackPaths(userID)
	if err != nil {
		return h.fail(c, err)
	}
	return ok(c, http.StatusOK, paths)
}

// placeDatapackRequest is the body of POST
// /datapacks/:datapack_id/placement.
type placeDatapackRequest struct {
	FolderID string `json:"folder_id"`
}

// PlaceDatapack handles POST /api/v1/datapacks/:datapack_id/placement —
// filing a datapack into a folder, or moving it to a different one.
//
// Guarded by RequireDatapackAccess rather than ownership: filing a datapack
// that was SHARED with you is the point, and the placement lands in your tree
// without touching the owner's.
//
//	@Summary	File or move a datapack into a folder
//	@Tags		collections
//	@Accept		json
//	@Produce	json
//	@Param		datapack_id	path		string	true	"Datapack ID"
//	@Success	200			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/placement [post]
func (h *CollectionHandler) PlaceDatapack(c echo.Context) error {
	userID, err := accountID(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
	}
	var req placeDatapackRequest
	if bErr := c.Bind(&req); bErr != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	datapackID := c.Param("datapack_id")
	if err = h.svc.Place(userID, datapackID, req.FolderID); err != nil {
		return h.fail(c, err)
	}
	return ok(c, http.StatusOK, map[string]string{"datapack_id": datapackID})
}

// UnplaceDatapack handles DELETE /api/v1/datapacks/:datapack_id/placement,
// returning the datapack to the unfiled set in the caller's tree.
//
//	@Summary	Remove a datapack from the caller's tree
//	@Tags		collections
//	@Produce	json
//	@Param		datapack_id	path		string	true	"Datapack ID"
//	@Success	200			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/placement [delete]
func (h *CollectionHandler) UnplaceDatapack(c echo.Context) error {
	userID, err := accountID(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
	}
	datapackID := c.Param("datapack_id")
	if err = h.svc.Unplace(userID, datapackID); err != nil {
		return h.fail(c, err)
	}
	return ok(c, http.StatusOK, map[string]string{"datapack_id": datapackID})
}
