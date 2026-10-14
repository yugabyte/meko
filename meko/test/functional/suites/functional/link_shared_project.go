package functional

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:       "functional/link_shared_project",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamAPIServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Description: "An owner files one datapack in each of two folders of a project and shares one through its link, with MEKO_ENABLE_LINK_SHARING on " +
			"(skipped when the stack leaves it off). The account that redeems the link sees the project under shared, named with the owner's email " +
			"and showing only the folder and datapack it can reach, and cannot rename it. Once the owner removes it, the project is gone from its list and 404.",
		Run: linkSharedProject,
	})
}

type sharedProject struct {
	ID            string   `json:"project_id"`
	OwnerEmail    string   `json:"owner_email"`
	FolderNames   []string `json:"folder_names"`
	DatapackCount int      `json:"datapack_count"`
}

type sharedProjectsList struct {
	Shared []sharedProject `json:"shared"`
}

func linkSharedProject(t *core.T) error {
	accts, done := accountsFor(t, 2)
	defer done()
	var us [2]*user
	for i := range us {
		u, err := newUser(t, accts[i])
		defer u.close()
		if err != nil {
			return err
		}
		us[i] = u
	}
	owner, joiner := us[0], us[1]
	sfx := t.ResourceSuffix()

	shared, err := ownedDatapack(t, owner, "dp-linkproj-"+sfx)
	defer deleteDatapack(owner, shared.ID)
	if err != nil {
		return err
	}
	private, err := ownedDatapack(t, owner, "dp-linkproj-private-"+sfx)
	defer deleteDatapack(owner, private.ID)
	if err != nil {
		return err
	}

	r, err := owner.api.Raw(t, http.MethodGet, "/datapacks/"+shared.ID+"/share-link", nil)
	if err != nil {
		return err
	}
	if r.Status == http.StatusNotFound {
		t.Skipf("the share-link routes are absent: MEKO_ENABLE_LINK_SHARING is off on this stack")
	}

	const sharedFolder, privateFolder = "shared", "private"
	first, err := newProject(t, owner, "linkproj-"+sfx, sharedFolder, privateFolder)
	projectID := first.ProjectID
	defer func() {
		if projectID == "" {
			return
		}
		ctx, cancel := cleanupCtx()
		defer cancel()
		_, _ = owner.api.Raw(ctx, del, "/projects/"+projectID, nil)
	}()
	if err != nil {
		return err
	}
	var detail struct {
		Folders []projectFolder `json:"folders"`
	}
	if err = owner.rest(get, "/projects/"+projectID, nil, httpOK, &detail); err != nil {
		return err
	}
	folderID := map[string]string{}
	for _, f := range detail.Folders {
		folderID[f.Name] = f.ID
	}
	for dp, folder := range map[string]string{shared.ID: sharedFolder, private.ID: privateFolder} {
		if err = owner.rest(post, "/datapacks/"+dp+"/placement", map[string]string{"folder_id": folderID[folder]}, httpOK, nil); err != nil {
			return fmt.Errorf("file %s in %s: %w", dp, folder, err)
		}
	}

	link, err := owner.api.UpdateShareLink(t, shared.ID, mekoapi.GrantViewer, mekoapi.ShareLinkActive)
	if err != nil {
		return err
	}
	if _, err := joiner.api.RedeemShareLink(t, link.Token); err != nil {
		return err
	}

	listShared := func() (sharedProjectsList, error) {
		var l sharedProjectsList
		err := joiner.rest(get, "/projects", nil, httpOK, &l)
		return l, err
	}

	if err := t.Step("joiner_sees_the_project_scoped_to_what_it_can_reach", func(*core.StepHandle) error {
		l, err := listShared()
		if err != nil {
			return err
		}
		i := slices.IndexFunc(l.Shared, func(p sharedProject) bool { return p.ID == projectID })
		if i < 0 {
			return fmt.Errorf("after redeeming the link of %s the joiner's shared projects are %+v, want %s among them", shared.ID, l.Shared, projectID)
		}
		p := l.Shared[i]
		if p.OwnerEmail != owner.acct.Email {
			return fmt.Errorf("the shared project names owner %q, want %q", p.OwnerEmail, owner.acct.Email)
		}
		if !slices.Equal(p.FolderNames, []string{sharedFolder}) || p.DatapackCount != 1 {
			return fmt.Errorf("the shared project shows folders %v and %d datapacks, want only [%s] and 1", p.FolderNames, p.DatapackCount, sharedFolder)
		}
		var d struct {
			Folders   []projectFolder  `json:"folders"`
			Datapacks []map[string]any `json:"datapacks"`
		}
		if err := joiner.rest(get, "/projects/"+projectID, nil, httpOK, &d); err != nil {
			return err
		}
		for _, f := range d.Folders {
			if f.Name == privateFolder {
				return fmt.Errorf("the joiner's project detail shows folder %q, which holds nothing it can reach", privateFolder)
			}
		}
		for _, dp := range d.Datapacks {
			if dp["datapack_id"] == private.ID {
				return fmt.Errorf("the joiner's project detail shows datapack %s, which was never shared", private.ID)
			}
		}
		return nil
	}); err != nil {
		return err
	}

	if err := t.Step("joiner_cannot_rename_the_project", func(*core.StepHandle) error {
		rename := map[string]any{"project_name": "hijack-" + sfx}
		if err := refused(t, "the joiner renames the owner's project", joiner.api, patch, "/projects/"+projectID, rename, http.StatusNotFound, ""); err != nil {
			return refused(t, "the joiner renames the owner's project", joiner.api, patch, "/projects/"+projectID, rename, http.StatusForbidden, "")
		}
		return nil
	}); err != nil {
		return err
	}

	return t.Step("removal_hides_the_project", func(*core.StepHandle) error {
		if err := owner.api.RemoveMember(t, shared.ID, joiner.acct.ID); err != nil {
			return err
		}
		l, err := listShared()
		if err != nil {
			return err
		}
		for _, p := range l.Shared {
			if p.ID == projectID {
				return fmt.Errorf("after removal the joiner still lists project %s as shared", projectID)
			}
		}
		return refused(t, "the removed joiner reading the project", joiner.api, get, "/projects/"+projectID, nil, statusNotFound, "")
	})
}
