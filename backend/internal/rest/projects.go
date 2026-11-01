package rest

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/yugabyte/meko/backend/internal/envelope"
	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/wire"
)

// Projects: Meko Cloud's routes and shapes for filing this Meko's datapacks in projects and their
// folders (memory/projects.go). They're this Meko's own while Meko Cloud is linked too: a project
// files only this Meko's datapacks, and Meko Cloud's projects are in its web app.

// maxPlacementIDs is the most datapacks GET /projects/placements takes, Cloud's limit.
const maxPlacementIDs = 1000

func (a *api) registerProjects(g *echo.Group) {
	g.GET("/projects/themes", a.projectThemes)
	g.GET("/projects/placements", a.placements)
	g.GET("/projects", a.listProjects)
	g.POST("/projects", a.createProject)
	g.GET("/projects/:project", a.project)
	g.PATCH("/projects/:project", a.updateProject)
	g.DELETE("/projects/:project", a.deleteProject)
	g.GET("/projects/:project/activity-log", a.projectActivity)
	g.GET("/projects/:project/stats", a.projectStats)
	g.POST("/projects/:project/folders", a.createFolder)
	g.PATCH("/projects/:project/folders/:folder", a.updateFolder)
	g.DELETE("/projects/:project/folders/:folder", a.deleteFolder)
	g.POST("/datapacks/:id/placement", a.placeDatapack)
	g.DELETE("/datapacks/:id/placement", a.unplaceDatapack)
}

func projectError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, memory.ErrNoProject), errors.Is(err, memory.ErrNoFolder):
		return notFound(c, err.Error())
	case errors.Is(err, memory.ErrNameTaken):
		return envelope.Fail(c, http.StatusConflict, "CONFLICT",
			strings.TrimPrefix(err.Error(), memory.ErrNameTaken.Error()+": "))
	}
	return serviceError(c, err)
}

func (a *api) projectThemes(c echo.Context) error { return envelope.OK(c, memory.Themes) }

// listProjects is {owned, shared}.
func (a *api) listProjects(c echo.Context) error {
	ps, err := a.Service.Projects(ctx(c))
	if err != nil {
		return err
	}
	return envelope.OK(c, wire.ProjectList(ps))
}

// createProject is POST /projects {project_name, description, theme_key, folders}.
func (a *api) createProject(c echo.Context) error {
	var body struct {
		ProjectName string   `json:"project_name"`
		Description string   `json:"description"`
		ThemeKey    string   `json:"theme_key"`
		Folders     []string `json:"folders"`
	}
	if ok, berr := bind(c, &body); !ok {
		return berr
	}
	p, err := a.Service.CreateProject(ctx(c), memory.NewProject{Name: body.ProjectName,
		Description: body.Description, ThemeKey: body.ThemeKey, Folders: body.Folders})
	if err != nil {
		return projectError(c, err)
	}
	return created(c, wire.ProjectOf(p))
}

// project is GET /projects/:project: {project, folders, datapacks, members}.
func (a *api) project(c echo.Context) error {
	id := c.Param("project")
	p, err := a.Service.Project(ctx(c), id)
	if err != nil {
		return projectError(c, err)
	}
	folders, filed, err := a.Service.ProjectTree(ctx(c), id)
	if err != nil {
		return projectError(c, err)
	}
	return envelope.OK(c, wire.ProjectDetailOf(p, folders, filed))
}

// updateProject is PATCH /projects/:project {project_name, description}: what's sent changes.
func (a *api) updateProject(c echo.Context) error {
	var body struct {
		ProjectName *string `json:"project_name"`
		Description *string `json:"description"`
	}
	if ok, berr := bind(c, &body); !ok {
		return berr
	}
	id := c.Param("project")
	if err := a.Service.UpdateProject(ctx(c), id, body.ProjectName, body.Description); err != nil {
		return projectError(c, err)
	}
	return envelope.OK(c, map[string]string{"project_id": id})
}

// deleteProject is DELETE /projects/:project: its folders go, and its datapacks are unfiled.
func (a *api) deleteProject(c echo.Context) error {
	id := c.Param("project")
	if err := a.Service.DeleteProject(ctx(c), id); err != nil {
		return projectError(c, err)
	}
	return envelope.OK(c, map[string]string{"project_id": id})
}

// projectActivity is GET /projects/:project/activity-log?limit=&cursor=, a datapack's activity log's
// shape with folder_created and datapack_filed events too.
func (a *api) projectActivity(c echo.Context) error {
	limit, ok, err := queryInt(c, "limit", activityPage)
	if !ok {
		return err
	}
	limit = min(max(limit, 1), maxActivityPage)
	acts, next, err := a.Service.ProjectActivity(ctx(c), c.Param("project"), c.QueryParam("cursor"), limit)
	if err != nil {
		return projectError(c, err)
	}
	email := a.email(c)
	items := make([]wire.ActivityItem, len(acts))
	for i, x := range acts {
		items[i] = wire.ProjectActivityItemOf(x, email)
	}
	return envelope.OK(c, map[string]any{"items": items, "has_more": next != "", "next_cursor": next,
		"limit": limit})
}

// projectStats is GET /projects/:project/stats: what its datapacks hold in all.
func (a *api) projectStats(c echo.Context) error {
	st, err := a.Service.ProjectStats(ctx(c), c.Param("project"))
	if err != nil {
		return projectError(c, err)
	}
	return envelope.OK(c, wire.ProjectStatsOf(st))
}

type folderBody struct {
	FolderName        *string `json:"folder_name"`
	FolderDescription *string `json:"folder_description"`
}

// createFolder is POST /projects/:project/folders {folder_name, folder_description}.
func (a *api) createFolder(c echo.Context) error {
	var body folderBody
	if ok, berr := bind(c, &body); !ok {
		return berr
	}
	name, desc := "", ""
	if body.FolderName != nil {
		name = *body.FolderName
	}
	if body.FolderDescription != nil {
		desc = *body.FolderDescription
	}
	f, err := a.Service.CreateFolder(ctx(c), c.Param("project"), name, desc)
	if err != nil {
		return projectError(c, err)
	}
	return created(c, wire.FolderOf(f))
}

// updateFolder is PATCH /projects/:project/folders/:folder {folder_name, folder_description}.
func (a *api) updateFolder(c echo.Context) error {
	var body folderBody
	if ok, berr := bind(c, &body); !ok {
		return berr
	}
	id := c.Param("folder")
	if err := a.Service.UpdateFolder(ctx(c), c.Param("project"), id, body.FolderName,
		body.FolderDescription); err != nil {
		return projectError(c, err)
	}
	return envelope.OK(c, map[string]string{"folder_id": id})
}

// deleteFolder is DELETE /projects/:project/folders/:folder: its datapacks are unfiled.
func (a *api) deleteFolder(c echo.Context) error {
	id := c.Param("folder")
	if err := a.Service.DeleteFolder(ctx(c), c.Param("project"), id); err != nil {
		return projectError(c, err)
	}
	return envelope.OK(c, map[string]string{"folder_id": id})
}

// placeDatapack is POST /datapacks/:id/placement {folder_id}: files one of this Meko's datapacks
// there, moving it from where it was.
func (a *api) placeDatapack(c echo.Context) error {
	dp, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	var body struct {
		FolderID string `json:"folder_id"`
	}
	if ok, berr := bind(c, &body); !ok {
		return berr
	}
	if err := a.Service.PlaceDatapack(ctx(c), dp, body.FolderID); err != nil {
		return projectError(c, err)
	}
	return envelope.OK(c, map[string]string{"datapack_id": dp})
}

// unplaceDatapack is DELETE /datapacks/:id/placement: the datapack is unfiled.
func (a *api) unplaceDatapack(c echo.Context) error {
	dp, ok, err := a.pathDatapack(c)
	if !ok {
		return err
	}
	if err := a.Service.UnplaceDatapack(ctx(c), dp); err != nil {
		return projectError(c, err)
	}
	return envelope.OK(c, map[string]string{"datapack_id": dp})
}

// placements is GET /projects/placements?datapack_ids=a,b: {id: placement} for the filed ones among
// them (every filed one, without the parameter).
func (a *api) placements(c echo.Context) error {
	var ids []string
	if q := c.QueryParam("datapack_ids"); q != "" {
		for _, id := range strings.Split(q, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, memory.CanonicalID(id))
			}
		}
		if len(ids) > maxPlacementIDs {
			return envelope.Fail(c, http.StatusBadRequest, "INVALID_DATAPACK_IDS",
				"datapack_ids takes up to 1000 ids")
		}
	}
	ps, err := a.Service.Placements(ctx(c), ids)
	if err != nil {
		return err
	}
	out := make(map[string]wire.Placement, len(ps))
	for id, p := range ps {
		out[id] = wire.PlacementOf(p)
	}
	return envelope.OK(c, out)
}
