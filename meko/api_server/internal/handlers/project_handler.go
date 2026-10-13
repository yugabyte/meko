package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// ProjectHandler holds Echo handlers for the project tree: projects, their
// folders, and where datapacks sit inside them.
type ProjectHandler struct {
	svc *service.ProjectService
}

// NewProjectHandler creates a new ProjectHandler.
func NewProjectHandler(svc *service.ProjectService) *ProjectHandler {
	return &ProjectHandler{svc: svc}
}

// projectStatus maps a service/repository error to its HTTP answer.
//
// One place rather than a repeated switch per handler: every project endpoint
// can produce the same four outcomes, and a name collision in particular has
// to be a 409 rather than a 500 so the UI can prompt for a different name.
func projectStatus(err error) (int, string) {
	switch {
	case errors.Is(err, service.ErrInvalidProjectName),
		errors.Is(err, service.ErrUnknownTheme):
		return http.StatusBadRequest, "VALIDATION_ERROR"
	case errors.Is(err, repository.ErrProjectNotFound),
		errors.Is(err, repository.ErrFolderNotFound),
		errors.Is(err, service.ErrFolderNotInProject):
		return http.StatusNotFound, "NOT_FOUND"
	case errors.Is(err, service.ErrNotDatapackOwner):
		return http.StatusForbidden, "FORBIDDEN"
	case errors.Is(err, repository.ErrProjectNameTaken),
		errors.Is(err, repository.ErrFolderNameTaken):
		return http.StatusConflict, "CONFLICT"
	default:
		return http.StatusInternalServerError, "INTERNAL_ERROR"
	}
}

func (h *ProjectHandler) fail(c echo.Context, err error) error {
	status, code := projectStatus(err)
	return apiError(c, status, code, err.Error())
}

// accountID resolves the caller. Every project route is JWT-only and
// user-scoped, so this guard is the same on all of them.
func accountID(c echo.Context) (string, error) {
	account, err := auth.GetAccount(c)
	if err != nil || account == nil {
		return "", errors.New("missing account context")
	}
	return account.ID, nil
}

// Themes handles GET /api/v1/projects/themes.
//
// Served from Go constants, not a table: a theme is stamped into a project's
// folders at creation and never read again. The UI renders the picker from
// this so there is one source of truth for the layouts.
//
//	@Summary	List project themes
//	@Tags		projects
//	@Produce	json
//	@Success	200	{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/projects/themes [get]
func (h *ProjectHandler) Themes(c echo.Context) error {
	return ok(c, http.StatusOK, service.ProjectThemes())
}

// List handles GET /api/v1/projects.
//
//	@Summary	List projects with folder and datapack counts
//	@Tags		projects
//	@Produce	json
//	@Success	200	{object}	APIResponse
//	@Failure	401	{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/projects [get]
func (h *ProjectHandler) List(c echo.Context) error {
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
	// and a recipient can do far less with a shared project, so keeping them
	// apart on the wire stops a client treating them alike by accident.
	return ok(c, http.StatusOK, projectListResponse{Owned: owned, Shared: shared})
}

// projectListResponse is the body of GET /projects.
type projectListResponse struct {
	Owned  []models.Project `json:"owned"`
	Shared []models.Project `json:"shared"`
}

// Page size for the project activity feed. Shared with the per-datapack
// endpoint's own defaults so "Show more" behaves the same on both.
const (
	activityLogDefaultLimit = 10
	activityLogMaxLimit     = 50
)

// placementsMaxIDs caps datapack_ids on GET /projects/placements.
const placementsMaxIDs = 1000

// ActivityLog handles GET /api/v1/projects/:project_id/activity-log.
//
//	@Summary	Uploads, promotions, folder creations and filings across a project, newest first
//	@Tags		projects
//	@Produce	json
//	@Param		project_id	path		string	true	"Project ID"
//	@Param		limit		query		int		false	"Page size (default 10, max 50)"
//	@Param		cursor		query		string	false	"Opaque cursor from the previous page"
//	@Success	200			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/projects/{project_id}/activity-log [get]
func (h *ProjectHandler) ActivityLog(c echo.Context) error {
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
		userID, c.Param("project_id"), limit, strings.TrimSpace(c.QueryParam("cursor")),
	)
	if err != nil {
		return h.fail(c, err)
	}
	return ok(c, http.StatusOK, resp)
}

// Stats handles GET /api/v1/projects/:project_id/stats.
//
//	@Summary	What a project holds
//	@Tags		projects
//	@Produce	json
//	@Param		project_id	path		string	true	"Project ID"
//	@Success	200			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/projects/{project_id}/stats [get]
func (h *ProjectHandler) Stats(c echo.Context) error {
	userID, err := accountID(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
	}

	resp, err := h.svc.Stats(userID, c.Param("project_id"))
	if err != nil {
		return h.fail(c, err)
	}
	return ok(c, http.StatusOK, resp)
}

// Create handles POST /api/v1/projects.
//
//	@Summary	Create a project from a theme
//	@Tags		projects
//	@Accept		json
//	@Produce	json
//	@Param		body	body		service.CreateProjectRequest	true	"Create request"
//	@Success	201		{object}	APIResponse
//	@Failure	400		{object}	APIResponse
//	@Failure	409		{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/projects [post]
func (h *ProjectHandler) Create(c echo.Context) error {
	userID, err := accountID(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
	}
	var req service.CreateProjectRequest
	if bErr := c.Bind(&req); bErr != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	project, err := h.svc.Create(userID, req)
	if err != nil {
		return h.fail(c, err)
	}
	return ok(c, http.StatusCreated, project)
}

// Detail handles GET /api/v1/projects/:project_id — the project and its
// folder tree, each folder carrying its own datapack count.
//
//	@Summary	Get a project, its folders, and the datapacks in them with counts
//	@Tags		projects
//	@Produce	json
//	@Param		project_id	path		string	true	"Project ID"
//	@Success	200			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/projects/{project_id} [get]
func (h *ProjectHandler) Detail(c echo.Context) error {
	userID, err := accountID(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
	}
	detail, err := h.svc.Detail(userID, c.Param("project_id"))
	if err != nil {
		return h.fail(c, err)
	}
	return ok(c, http.StatusOK, detail)
}

// updateProjectRequest is the body of PATCH /projects/:project_id.
type updateProjectRequest struct {
	Name        string `json:"project_name"`
	Description string `json:"description"`
}

// Update handles PATCH /api/v1/projects/:project_id.
//
//	@Summary	Rename a project
//	@Tags		projects
//	@Accept		json
//	@Produce	json
//	@Param		project_id	path		string	true	"Project ID"
//	@Success	200			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Failure	409			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/projects/{project_id} [patch]
func (h *ProjectHandler) Update(c echo.Context) error {
	userID, err := accountID(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
	}
	var req updateProjectRequest
	if bErr := c.Bind(&req); bErr != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	if err = h.svc.Rename(userID, c.Param("project_id"), req.Name, req.Description); err != nil {
		return h.fail(c, err)
	}
	return ok(c, http.StatusOK, map[string]string{"project_id": c.Param("project_id")})
}

// Delete handles DELETE /api/v1/projects/:project_id.
//
// The datapacks inside are NOT deleted — they become unfiled. That is a
// property of the schema (placements cascade, datapacks don't), and it is what
// makes deleting a project recoverable by refiling.
//
//	@Summary	Delete a project
//	@Tags		projects
//	@Produce	json
//	@Param		project_id	path		string	true	"Project ID"
//	@Success	200			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/projects/{project_id} [delete]
func (h *ProjectHandler) Delete(c echo.Context) error {
	userID, err := accountID(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
	}
	if err = h.svc.Delete(userID, c.Param("project_id")); err != nil {
		return h.fail(c, err)
	}
	return ok(c, http.StatusOK, map[string]string{"project_id": c.Param("project_id")})
}

// createFolderRequest is the body of POST /projects/:project_id/folders.
type createFolderRequest struct {
	Name string `json:"folder_name"`
	// Optional. What the folder is for, which its name often cannot say.
	Description string `json:"folder_description"`
}

// CreateFolder handles POST /api/v1/projects/:project_id/folders.
//
//	@Summary	Add a folder to a project
//	@Tags		projects
//	@Accept		json
//	@Produce	json
//	@Param		project_id	path		string	true	"Project ID"
//	@Success	201			{object}	APIResponse
//	@Failure	400			{object}	APIResponse
//	@Failure	409			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/projects/{project_id}/folders [post]
func (h *ProjectHandler) CreateFolder(c echo.Context) error {
	userID, err := accountID(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
	}
	var req createFolderRequest
	if bErr := c.Bind(&req); bErr != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	folder, err := h.svc.CreateFolder(userID, c.Param("project_id"), req.Name, req.Description)
	if err != nil {
		return h.fail(c, err)
	}
	return ok(c, http.StatusCreated, folder)
}

// updateFolderRequest is the body of PATCH
// /projects/:project_id/folders/:folder_id.
type updateFolderRequest struct {
	Name string `json:"folder_name"`
	// Pointer so an omitted field is distinguishable from an explicit "":
	// omitted leaves the stored description alone, "" clears it. A
	// rename-only PATCH -- an older UI bundle, a PAT client, an agent -- must
	// not wipe a description it never sent.
	Description *string `json:"folder_description"`
}

// UpdateFolder handles PATCH /api/v1/projects/:project_id/folders/:folder_id.
//
//	@Summary	Rename a folder
//	@Tags		projects
//	@Accept		json
//	@Produce	json
//	@Param		project_id	path		string	true	"Project ID"
//	@Param		folder_id	path		string	true	"Folder ID"
//	@Success	200			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Failure	409			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/projects/{project_id}/folders/{folder_id} [patch]
func (h *ProjectHandler) UpdateFolder(c echo.Context) error {
	userID, err := accountID(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
	}
	var req updateFolderRequest
	if bErr := c.Bind(&req); bErr != nil {
		return apiError(c, http.StatusBadRequest, "INVALID_JSON", "malformed request body")
	}
	if err = h.svc.RenameFolder(
		userID, c.Param("project_id"), c.Param("folder_id"), req.Name, req.Description,
	); err != nil {
		return h.fail(c, err)
	}
	return ok(c, http.StatusOK, map[string]string{"folder_id": c.Param("folder_id")})
}

// DeleteFolder handles DELETE
// /api/v1/projects/:project_id/folders/:folder_id.
//
//	@Summary	Delete a folder and its subfolders
//	@Tags		projects
//	@Produce	json
//	@Param		project_id	path		string	true	"Project ID"
//	@Param		folder_id	path		string	true	"Folder ID"
//	@Success	200			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/projects/{project_id}/folders/{folder_id} [delete]
func (h *ProjectHandler) DeleteFolder(c echo.Context) error {
	userID, err := accountID(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
	}
	if err = h.svc.DeleteFolder(userID, c.Param("project_id"), c.Param("folder_id")); err != nil {
		return h.fail(c, err)
	}
	return ok(c, http.StatusOK, map[string]string{"folder_id": c.Param("folder_id")})
}

// DatapackPaths handles GET /api/v1/projects/placements — where every filed
// datapack sits in the caller's tree, keyed by datapack_id.
//
// One call labels the whole Datapacks page. A datapack missing from the map is
// unfiled, so the client needs no second request to tell the two apart.
//
// datapack_ids (comma-separated, at most 1000) limits the map to those
// datapacks, for a client showing a page of a very large account.
//
//	@Summary	Map every filed datapack to its path
//	@Tags		projects
//	@Produce	json
//	@Param		datapack_ids	query		string	false	"Comma-separated datapack ids to limit the map to (max 1000)"
//	@Success	200	{object}	APIResponse
//	@Failure	400	{object}	APIResponse
//	@Failure	401	{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/projects/placements [get]
func (h *ProjectHandler) DatapackPaths(c echo.Context) error {
	userID, err := accountID(c)
	if err != nil {
		return apiError(c, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
	}
	var ids []string
	if c.QueryParams().Has("datapack_ids") {
		ids = []string{}
		for _, raw := range strings.Split(c.QueryParam("datapack_ids"), ",") {
			id := strings.TrimSpace(raw)
			if id == "" {
				continue
			}
			parsed, pErr := uuid.Parse(id)
			if pErr != nil {
				return apiError(c, http.StatusBadRequest, "INVALID_DATAPACK_IDS", "datapack_ids must be UUIDs")
			}
			// String() is the lowercase form SharedIDs returns. The raw
			// value can be uppercase, which ::uuid accepts and a string
			// compare does not.
			ids = append(ids, parsed.String())
		}
		if len(ids) > placementsMaxIDs {
			return apiError(c, http.StatusBadRequest, "INVALID_DATAPACK_IDS", "at most 1000 datapack_ids")
		}
	}
	paths, err := h.svc.DatapackPaths(userID, ids)
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
//	@Tags		projects
//	@Accept		json
//	@Produce	json
//	@Param		datapack_id	path		string	true	"Datapack ID"
//	@Success	200			{object}	APIResponse
//	@Failure	404			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/placement [post]
func (h *ProjectHandler) PlaceDatapack(c echo.Context) error {
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
//	@Tags		projects
//	@Produce	json
//	@Param		datapack_id	path		string	true	"Datapack ID"
//	@Success	200			{object}	APIResponse
//	@Security	BearerAuth
//	@Router		/datapacks/{datapack_id}/placement [delete]
func (h *ProjectHandler) UnplaceDatapack(c echo.Context) error {
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
