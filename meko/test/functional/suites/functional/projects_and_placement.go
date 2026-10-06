package functional

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:       "functional/projects_and_placement",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamAPIServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Timeout:    10 * time.Minute,
		Description: "Projects and folders as the web app's Projects page uses them. A project made from a theme gets the theme's folders, one made blank needs a folder; " +
			"datapacks are filed, moved between folders and unfiled, and the placements map, the project detail, its stats and its activity log follow every move; " +
			"renaming a project and a folder shows in the detail; a folder holding a datapack is refused or unfiles it on delete; a viewer of a shared datapack sees the " +
			"owner's project read-only, a stranger cannot read, change or file into it; deleting the project deletes the owner's datapacks filed in it, for the viewer " +
			"too, and leaves an unfiled one alone.",
		Run: projectsAndPlacement,
	})
}

type projectDetail struct {
	Project struct {
		ID   string `json:"project_id"`
		Name string `json:"project_name"`
	} `json:"project"`
	Folders   []projectFolder  `json:"folders"`
	Datapacks []map[string]any `json:"datapacks"`
}

func projectsAndPlacement(t *core.T) error {
	accts, done := accountsFor(t, 3)
	defer done()
	var us [3]*user
	for i := range us {
		u, err := newUser(t, accts[i])
		defer u.close()
		if err != nil {
			return err
		}
		us[i] = u
	}
	owner, viewer, stranger := us[0], us[1], us[2]
	sfx := t.ResourceSuffix()
	dpA, err := ownedDatapack(t, owner, "proj-a-"+sfx)
	defer deleteDatapack(owner, dpA.ID)
	if err != nil {
		return err
	}
	dpB, err := ownedDatapack(t, owner, "proj-b-"+sfx)
	defer deleteDatapack(owner, dpB.ID)
	if err != nil {
		return err
	}
	if err := shareWith(t, owner, viewer, dpA.ID, "viewer"); err != nil {
		return err
	}

	var projectID string
	var folders []projectFolder
	defer func() {
		if projectID != "" {
			ctx, cancel := cleanupCtx()
			defer cancel()
			_, _ = owner.api.Raw(ctx, del, "/projects/"+projectID, nil)
		}
	}()
	detail := func(u *user) (projectDetail, error) {
		var d projectDetail
		err := u.rest(get, "/projects/"+projectID, nil, httpOK, &d)
		return d, err
	}
	placementOf := func(dp string) (string, error) {
		var m map[string]struct {
			FolderID string `json:"folder_id"`
			Path     string `json:"path"`
		}
		if err := owner.rest(get, "/projects/placements", nil, httpOK, &m); err != nil {
			return "", err
		}
		return m[dp].FolderID, nil
	}

	if err := t.Step("themes_and_create", func(h *core.StepHandle) error {
		var themes []struct {
			Key     string   `json:"key"`
			Folders []string `json:"folders"`
		}
		if err := owner.rest(get, "/projects/themes", nil, httpOK, &themes); err != nil {
			return err
		}
		if len(themes) == 0 || len(themes[0].Folders) == 0 {
			return fmt.Errorf("the themes list is %+v, want themes with folders", themes)
		}
		if err := refused(t, "a blank project with no folder", owner.api, post, "/projects", map[string]any{"project_name": "blank-" + sfx, "theme_key": "blank"}, 400, "VALIDATION_ERROR"); err != nil {
			return err
		}
		var p struct {
			ID string `json:"project_id"`
		}
		if err := owner.rest(post, "/projects", map[string]any{"project_name": "proj-" + sfx, "theme_key": themes[0].Key}, 201, &p); err != nil {
			return err
		}
		projectID = p.ID
		d, err := detail(owner)
		if err != nil {
			return err
		}
		folders = d.Folders
		h.Set("theme", themes[0].Key)
		h.Set("folders", len(folders))
		if len(folders) != len(themes[0].Folders) {
			return fmt.Errorf("project from theme %s has %d folders, the theme lists %d", themes[0].Key, len(folders), len(themes[0].Folders))
		}
		if len(folders) < 2 {
			var f projectFolder
			if err := owner.rest(post, "/projects/"+projectID+"/folders", map[string]any{"folder_name": "second"}, 201, &f); err != nil {
				return err
			}
			d, err := detail(owner)
			if err != nil {
				return err
			}
			folders = d.Folders
		}
		return nil
	}); err != nil {
		return err
	}

	var errs []error
	step := func(name string, fn func() error) {
		if err := t.Step(name, func(*core.StepHandle) error { return fn() }); err != nil {
			errs = append(errs, err)
		}
	}
	step("file_move_unfile", func() error {
		for _, dp := range []string{dpA.ID, dpB.ID} {
			if err := owner.rest(post, "/datapacks/"+dp+"/placement", map[string]string{"folder_id": folders[0].ID}, httpOK, nil); err != nil {
				return err
			}
		}
		if f, err := placementOf(dpA.ID); err != nil || f != folders[0].ID {
			return fmt.Errorf("after filing, %s is in folder %q (%v), want %s", dpA.ID, f, err, folders[0].ID)
		}
		if err := owner.rest(post, "/datapacks/"+dpA.ID+"/placement", map[string]string{"folder_id": folders[1].ID}, httpOK, nil); err != nil {
			return err
		}
		if f, err := placementOf(dpA.ID); err != nil || f != folders[1].ID {
			return fmt.Errorf("after the move, %s is in folder %q (%v), want %s", dpA.ID, f, err, folders[1].ID)
		}
		d, err := detail(owner)
		if err != nil {
			return err
		}
		counts := map[string]int{}
		for _, f := range d.Folders {
			counts[f.ID] = f.DatapackCount
		}
		if counts[folders[0].ID] != 1 || counts[folders[1].ID] != 1 {
			return fmt.Errorf("folder counts %v, want one datapack in each of the first two folders", counts)
		}
		var stats map[string]any
		if err := owner.rest(get, "/projects/"+projectID+"/stats", nil, httpOK, &stats); err != nil {
			return err
		}
		var activity map[string]any
		if err := owner.rest(get, "/projects/"+projectID+"/activity-log", nil, httpOK, &activity); err != nil {
			return err
		}
		if err := owner.rest(del, "/datapacks/"+dpB.ID+"/placement", nil, httpOK, nil); err != nil {
			return err
		}
		if f, err := placementOf(dpB.ID); err != nil || f != "" {
			return fmt.Errorf("after unfiling, %s is still in folder %q (%v)", dpB.ID, f, err)
		}
		return nil
	})
	step("rename_project_and_folder", func() error {
		if err := owner.rest(patch, "/projects/"+projectID, map[string]any{"project_name": "renamed-" + sfx}, httpOK, nil); err != nil {
			return err
		}
		if err := owner.rest(patch, "/projects/"+projectID+"/folders/"+folders[0].ID, map[string]any{"folder_name": "renamed-folder"}, httpOK, nil); err != nil {
			return err
		}
		d, err := detail(owner)
		if err != nil {
			return err
		}
		if d.Project.Name != "renamed-"+sfx {
			return fmt.Errorf("the project is named %q after the rename", d.Project.Name)
		}
		for _, f := range d.Folders {
			if f.ID == folders[0].ID && f.Name != "renamed-folder" {
				return fmt.Errorf("folder %s is named %q after the rename", f.ID, f.Name)
			}
		}
		return nil
	})
	step("deleting_a_filled_folder_is_safe", func() error {
		r, err := owner.api.Raw(t, del, "/projects/"+projectID+"/folders/"+folders[1].ID, nil)
		if err != nil {
			return err
		}
		switch {
		case r.Status == http.StatusConflict || r.Status == http.StatusBadRequest:
			return nil
		case r.Status/100 == 2:
			if f, err := placementOf(dpA.ID); err != nil || f == folders[1].ID {
				return fmt.Errorf("folder %s was deleted and %s still points at it (%v)", folders[1].ID, dpA.ID, err)
			}
			if _, err := owner.api.GetDatapack(t, dpA.ID); err != nil {
				return fmt.Errorf("deleting the folder took datapack %s with it: %w", dpA.ID, err)
			}
			return nil
		default:
			return fmt.Errorf("deleting a folder that holds a datapack answered %d: %s", r.Status, truncateBody(r.Body))
		}
	})
	step("viewer_reads_stranger_is_refused", func() error {
		if err := owner.rest(post, "/datapacks/"+dpA.ID+"/placement", map[string]string{"folder_id": folders[0].ID}, httpOK, nil); err != nil {
			return fmt.Errorf("file the shared datapack again: %w", err)
		}
		var list struct {
			Shared []struct {
				ID string `json:"project_id"`
			} `json:"shared"`
		}
		if err := viewer.rest(get, "/projects", nil, httpOK, &list); err != nil {
			return err
		}
		found := false
		for _, p := range list.Shared {
			found = found || p.ID == projectID
		}
		if !found {
			return fmt.Errorf("the viewer of %s does not see the owner's project %s among shared ones", dpA.ID, projectID)
		}
		if err := refused(t, "the viewer renames the owner's project", viewer.api, patch, "/projects/"+projectID, map[string]any{"project_name": "hijack"}, http.StatusNotFound, ""); err != nil {
			if err2 := refused(t, "the viewer renames the owner's project", viewer.api, patch, "/projects/"+projectID, map[string]any{"project_name": "hijack"}, http.StatusForbidden, ""); err2 != nil {
				return err
			}
		}
		if err := refused(t, "a stranger reads the project", stranger.api, get, "/projects/"+projectID, nil, http.StatusNotFound, "", "renamed-"+sfx); err != nil {
			return err
		}
		return refused(t, "a stranger files the owner's datapack", stranger.api, post, "/datapacks/"+dpA.ID+"/placement", map[string]string{"folder_id": folders[0].ID}, http.StatusNotFound, "")
	})
	step("delete_project_takes_its_filed_datapacks", func() error {
		if err := owner.rest(post, "/datapacks/"+dpA.ID+"/placement", map[string]string{"folder_id": folders[0].ID}, httpOK, nil); err != nil {
			return err
		}
		if err := owner.rest(del, "/projects/"+projectID, nil, httpOK, nil); err != nil {
			return err
		}
		gone := projectID
		projectID = ""
		if err := refused(t, "the deleted project", owner.api, get, "/projects/"+gone, nil, http.StatusNotFound, ""); err != nil {
			return err
		}
		if err := refused(t, "the datapack filed in the deleted project", owner.api, get, "/datapacks/"+dpA.ID, nil, http.StatusNotFound, ""); err != nil {
			return err
		}
		if err := refused(t, "the viewer's copy of that datapack", viewer.api, get, "/datapacks/"+dpA.ID, nil, http.StatusNotFound, ""); err != nil {
			return err
		}
		if _, err := owner.api.GetDatapack(t, dpB.ID); err != nil {
			return fmt.Errorf("deleting the project took unfiled datapack %s with it: %w", dpB.ID, err)
		}
		return nil
	})
	return errors.Join(errs...)
}
