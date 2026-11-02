package mcp

import (
	"context"
	"errors"
	"strings"

	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/wire"
)

// The project tools: Meko Cloud's eleven project_* tools, with the same names,
// arguments and replies, over this Meko's projects. Every project here is the owner's, so none
// answers "not found" for a project shared with the caller.

const (
	hintProject = "Pass project_id (UUID). Use project_list to find it."
	hintFolder  = "Pass folder_id (UUID). Use project_describe to find it."
	hintFiled   = "Pass datapack_id (UUID). Use datapack_search(query=...) to find it."
)

func projectTools() []*tool {
	pTrace := param{name: "conversation_id", typ: "string", def: "",
		desc: "The conversation this call belongs to: the id conversation_create returned."}
	pProject := param{name: "project_id", typ: "string", required: true,
		desc: "The project's UUID. Use project_list to find it."}
	pFolder := param{name: "folder_id", typ: "string", required: true,
		desc: "The folder's UUID. Use project_describe to find it."}
	pFiled := param{name: "datapack_id", typ: "string", required: true, desc: "The datapack's UUID."}
	return []*tool{
		{
			name: "project_list", title: "List Projects", kind: readOnly, failCode: "project_list_failed",
			desc: "List the projects: each groups datapacks that work toward one outcome, in folders. Use it " +
				"to find a project_id before calling another project tool.",
			params: []param{pTrace},
			run:    projectList,
		},
		{
			name: "project_describe", title: "Describe Project", kind: readOnly, failCode: "project_describe_failed",
			desc: "Describe a project: its folders and the datapacks filed in each. This is how to find a " +
				"folder_id for project_add_datapack or datapack_create.",
			params: []param{pProject, pTrace},
			run:    projectDescribe,
		},
		{
			name: "project_stats", title: "Project Stats", kind: readOnly, failCode: "project_stats_failed",
			desc: "What a project's datapacks hold in all: searches, conversations, artifacts and knowledge-base " +
				"files. For its structure, use project_describe.",
			params: []param{pProject, pTrace},
			run:    projectStats,
		},
		{
			name: "project_create", title: "Create Project", kind: write, failCode: "project_create_failed",
			desc: "Create a project, with its first folders: one at least. Ask the user what the project is for " +
				"(the description) and which folders they want first: a folder is one kind or stage of work, " +
				"and folders don't nest.",
			params: []param{
				{name: "project_name", typ: "string", required: true, desc: "Its name: 2 to 60 characters, no /."},
				pTrace,
				{name: "description", typ: "string", def: "", desc: "What outcome the project serves."},
				{name: "folders", typ: "array", nullable: true, desc: "The names of its first folders: one at least."}},
			run: projectCreate,
		},
		{
			name: "project_update", title: "Update Project", kind: write, failCode: "project_update_failed",
			desc: "Rename a project, change its description, or both. Pass only what changes; " +
				`description="" clears it.`,
			params: []param{pProject, pTrace,
				{name: "project_name", typ: "string", nullable: true, desc: "The new name."},
				{name: "description", typ: "string", nullable: true, desc: "The new description."}},
			run: projectUpdate,
		},
		{
			name: "project_folder_create", title: "Create Project Folder", kind: write,
			failCode: "project_folder_create_failed",
			desc: "Add a folder to a project. Ask what belongs in it: its description is what tells a later " +
				"reader which folder a datapack goes in.",
			params: []param{pProject,
				{name: "folder_name", typ: "string", required: true, desc: "Its name, unique in the project."},
				pTrace,
				{name: "description", typ: "string", def: "", desc: "What belongs in the folder."}},
			run: projectFolderCreate,
		},
		{
			name: "project_folder_update", title: "Update Project Folder", kind: write,
			failCode: "project_folder_update_failed",
			desc: `Rename a folder, change its description, or both. Pass only what changes; description="" ` +
				"clears it.",
			params: []param{pProject, pFolder, pTrace,
				{name: "folder_name", typ: "string", nullable: true, desc: "The new name."},
				{name: "description", typ: "string", nullable: true, desc: "The new description."}},
			run: projectFolderUpdate,
		},
		{
			name: "project_folder_delete", title: "Delete Project Folder", kind: destructive,
			failCode: "project_folder_delete_failed",
			desc: "Delete a folder from a project. Its datapacks become Unfiled and keep working: this deletes " +
				"structure, not content. To delete a datapack, use datapack_delete.",
			params: []param{pProject, pFolder, pTrace},
			run:    projectFolderDelete,
		},
		{
			name: "project_add_datapack", title: "Add Datapack to Project", kind: write,
			failCode: "project_add_datapack_failed",
			desc: "File a datapack in a project's folder, or move it there: a datapack is in one project at " +
				"most. To create a datapack in a folder, pass folder_id to datapack_create instead.",
			params: []param{pFiled, pFolder, pTrace},
			run:    projectAddDatapack,
		},
		{
			name: "project_remove_datapack", title: "Remove Datapack from Project", kind: write,
			failCode: "project_remove_datapack_failed",
			desc:     "Take a datapack out of its project, leaving it Unfiled. It isn't deleted, and keeps working.",
			params:   []param{pFiled, pTrace},
			run:      projectRemoveDatapack,
		},
		{
			name: "project_delete", title: "Delete Project", kind: destructive, failCode: "project_delete_failed",
			desc: "Delete a project and its folders. Its datapacks aren't deleted: they become Unfiled and keep " +
				"working. It can't be undone, so confirm with the user first, naming the project.",
			params: []param{pProject, pTrace},
			run:    projectDelete,
		},
	}
}

// projectToolError is a project tool's error with the codes Cloud's API gives them.
func projectToolError(err error) error {
	switch {
	case errors.Is(err, memory.ErrNoProject), errors.Is(err, memory.ErrNoFolder):
		return fail("NOT_FOUND", strings.ToUpper(err.Error()[:1])+err.Error()[1:]+".")
	case errors.Is(err, memory.ErrNameTaken):
		return fail("CONFLICT", strings.TrimPrefix(err.Error(), memory.ErrNameTaken.Error()+": "))
	}
	return datapackError(err)
}

// need is a required argument, trimmed, or the {name}_required refusal with hint.
func need(a args, name, hint string) (string, error) {
	v, err := a.str(name)
	if err != nil {
		return "", err
	}
	if v = strings.TrimSpace(v); v == "" {
		return "", fail(name+"_required", hint)
	}
	return v, nil
}

func projectList(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	ps, err := c.svc.Projects(ctx)
	if err != nil {
		return nil, err
	}
	return wire.ProjectList(ps), nil
}

func projectDescribe(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	id, err := need(a, "project_id", hintProject)
	if err != nil {
		return nil, err
	}
	p, err := c.svc.Project(ctx, id)
	if err != nil {
		return nil, projectToolError(err)
	}
	folders, filed, err := c.svc.ProjectTree(ctx, id)
	if err != nil {
		return nil, projectToolError(err)
	}
	return wire.ProjectDetailOf(p, folders, filed), nil
}

func projectStats(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	id, err := need(a, "project_id", hintProject)
	if err != nil {
		return nil, err
	}
	st, err := c.svc.ProjectStats(ctx, id)
	if err != nil {
		return nil, projectToolError(err)
	}
	return wire.ProjectStatsOf(st), nil
}

func projectCreate(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	name, err := need(a, "project_name", "Pass project_name, the name of the project to create.")
	if err != nil {
		return nil, err
	}
	desc, err := a.str("description")
	if err != nil {
		return nil, err
	}
	list, err := a.strings("folders")
	if err != nil {
		return nil, err
	}
	var folders []string
	for _, f := range list {
		if f = strings.TrimSpace(f); f != "" {
			folders = append(folders, f)
		}
	}
	// No layout, as Cloud's tool sends none: its folders come from folders alone.
	p, err := c.svc.CreateProject(ctx, memory.NewProject{Name: name, Description: desc, Folders: folders})
	if err != nil {
		return nil, projectToolError(err)
	}
	return wire.ProjectOf(p), nil
}

// changes is a tool's name and description arguments, or nothing_to_update when it sends neither.
func changes(a args, nameArg string) (name, desc *string, err error) {
	if name, err = a.optStr(nameArg); err != nil {
		return nil, nil, err
	}
	if desc, err = a.optStr("description"); err != nil {
		return nil, nil, err
	}
	if name == nil && desc == nil {
		return nil, nil, fail("nothing_to_update",
			"Pass "+nameArg+`, description, or both. Pass description="" to clear it.`)
	}
	if desc != nil {
		d := strings.TrimSpace(*desc)
		desc = &d
	}
	return name, desc, nil
}

func projectUpdate(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	id, err := need(a, "project_id", hintProject)
	if err != nil {
		return nil, err
	}
	name, desc, err := changes(a, "project_name")
	if err != nil {
		return nil, err
	}
	if err = c.svc.UpdateProject(ctx, id, name, desc); err != nil {
		return nil, projectToolError(err)
	}
	return map[string]string{"project_id": id}, nil
}

func projectFolderCreate(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	id, err := need(a, "project_id", hintProject)
	if err != nil {
		return nil, err
	}
	name, err := need(a, "folder_name", "Pass folder_name, the name of the folder to create.")
	if err != nil {
		return nil, err
	}
	desc, err := a.str("description")
	if err != nil {
		return nil, err
	}
	f, err := c.svc.CreateFolder(ctx, id, name, desc)
	if err != nil {
		return nil, projectToolError(err)
	}
	return wire.FolderOf(f), nil
}

func projectFolderUpdate(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	id, err := need(a, "project_id", hintProject)
	if err != nil {
		return nil, err
	}
	folder, err := need(a, "folder_id", hintFolder)
	if err != nil {
		return nil, err
	}
	name, desc, err := changes(a, "folder_name")
	if err != nil {
		return nil, err
	}
	if err = c.svc.UpdateFolder(ctx, id, folder, name, desc); err != nil {
		return nil, projectToolError(err)
	}
	return map[string]string{"folder_id": folder}, nil
}

func projectFolderDelete(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	id, err := need(a, "project_id", hintProject)
	if err != nil {
		return nil, err
	}
	folder, err := need(a, "folder_id", hintFolder)
	if err != nil {
		return nil, err
	}
	if err = c.svc.DeleteFolder(ctx, id, folder); err != nil {
		return nil, projectToolError(err)
	}
	return map[string]string{"folder_id": folder}, nil
}

func projectAddDatapack(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	dp, err := need(a, "datapack_id", hintFiled)
	if err != nil {
		return nil, err
	}
	folder, err := need(a, "folder_id", hintFolder)
	if err != nil {
		return nil, err
	}
	dp = memory.CanonicalID(dp)
	c.rec.DatapackID = dp
	if err = c.svc.PlaceDatapack(ctx, dp, folder); err != nil {
		return nil, projectToolError(err)
	}
	return map[string]string{"datapack_id": dp}, nil
}

func projectRemoveDatapack(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	dp, err := need(a, "datapack_id", hintFiled)
	if err != nil {
		return nil, err
	}
	dp = memory.CanonicalID(dp)
	c.rec.DatapackID = dp
	if err = c.svc.UnplaceDatapack(ctx, dp); err != nil {
		return nil, projectToolError(err)
	}
	return map[string]string{"datapack_id": dp}, nil
}

func projectDelete(ctx context.Context, c *call, a args) (any, error) {
	if _, err := c.conversationID(a); err != nil {
		return nil, err
	}
	id, err := need(a, "project_id", hintProject)
	if err != nil {
		return nil, err
	}
	if err = c.svc.DeleteProject(ctx, id); err != nil {
		return nil, projectToolError(err)
	}
	return map[string]string{"project_id": id}, nil
}
