package functional

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

func init() {
	core.Register(core.Test{
		Name:       "functional/datapack_delete_cascade",
		Suite:      "functional",
		Category:   core.Functional,
		Tags:       tags(),
		OwningTeam: core.TeamAPIServer,
		Env:        core.EnvSpec{Sharing: core.Shared, Shape: core.SingleStack},
		Timeout:    20 * time.Minute,
		Description: "Deleting a datapack that holds everything a customer puts in one: memories, a conversation, an indexed document, an artifact, a member who accepted a share, " +
			"a pending invite and a project placement. After the delete the datapack answers 404 to its owner, is gone from the member's list and the invitee's incoming shares, " +
			"the pending invite cannot be accepted, MCP tools on it are refused without leaking its contents, its project placement is gone while the project stays, " +
			"no row in the datapack, memory, artifact, sharing, knowledge-base, placement or Langfuse-mapping tables still names it, and its name can be used again.",
		Run: datapackDeleteCascade,
	})
}

// cascadeTables are the tables that must hold nothing of a deleted datapack.
// History kept on purpose (token savings, daily activity, feedback) is not here.
var cascadeTables = []struct{ table, column string }{
	{"meko_system.datapacks", "datapack_id"},
	{"meko_system.agent_access", "datapack_id"},
	{"meko_system.agent_access_permissions", "datapack_id"},
	{"meko_system.datapack_invites", "datapack_id"},
	{"meko_system.datapack_placements", "datapack_id"},
	{"meko_system.knowledge_bases", "datapack_id"},
	{"meko_system.langfuse_project_mapping", "datapack_id"},
	{"meko_system.conversation_search_opt_outs", "datapack_id"},
	{"datapacks.meko_mem0_collection", "meko_datapack_id"},
	{"datapacks.promoted_collection", "meko_datapack_id"},
	{"datapacks.artifacts", "datapack_id"},
}

func datapackDeleteCascade(t *core.T) error {
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
	owner, member, invitee := us[0], us[1], us[2]
	mk := marker(t)
	name := "cascade-" + t.ResourceSuffix()
	dp, err := ownedDatapack(t, owner, name)
	deleted := false
	defer func() {
		if !deleted {
			deleteDatapack(owner, dp.ID)
		}
	}()
	if err != nil {
		return err
	}
	secret := fmt.Sprintf("The cascade secret %s is the word marigold.", mk)
	var projectID, inviteID, conv string

	if err := t.Step("fill_the_datapack", func(h *core.StepHandle) error {
		var err error
		if conv, err = owner.conversation(dp.ID, "cascade "+mk); err != nil {
			return err
		}
		if _, terr := owner.call("memory_add", map[string]any{"datapack_id": dp.ID, "conversation_id": conv, "text": secret}); terr != nil {
			return terr
		}
		if _, terr := owner.call("conversation_add_message", map[string]any{"datapack_id": dp.ID, "conversation_id": conv, "input": secret, "output": "Noted."}); terr != nil {
			return terr
		}
		if _, terr := owner.call("artifact_put", map[string]any{"datapack_id": dp.ID, "conversation_id": conv, "filename": "secret-" + mk + ".txt",
			"content_type": "text/plain", "content_base64": base64.StdEncoding.EncodeToString([]byte(secret))}); terr != nil {
			return terr
		}
		if err := shareWith(t, owner, member, dp.ID, "viewer"); err != nil {
			return err
		}
		inv, err := owner.api.ShareDatapack(t, dp.ID, invitee.acct.Email, "viewer")
		if err != nil {
			return err
		}
		inviteID = inv.InviteID
		folder, err := newProject(t, owner, "cascade-"+t.ResourceSuffix(), "keep")
		if err != nil {
			return err
		}
		projectID = folder.ProjectID
		if err := owner.rest(post, "/datapacks/"+dp.ID+"/placement", map[string]string{"folder_id": folder.ID}, httpOK, nil); err != nil {
			return err
		}
		h.Set("project_id", projectID)
		h.Set("invite_id", inviteID)
		return nil
	}); err != nil {
		return err
	}
	// A failed upload (meko#1673 on the default database image) costs this
	// check its document, not the rest of the cascade.
	kbErr := t.Step("add_a_document", func(*core.StepHandle) error {
		up, err := owner.api.UploadKBFile(t, dp.ID, "secret-"+mk+".txt", []byte(secret+"\n"))
		if err != nil {
			return err
		}
		return waitIndexed(t, owner.api, dp.ID, up.DocumentKey)
	})
	defer func() {
		if projectID != "" {
			ctx, cancel := cleanupCtx()
			defer cancel()
			_, _ = owner.api.Raw(ctx, del, "/projects/"+projectID, nil)
		}
	}()

	if err := t.Step("delete", func(*core.StepHandle) error {
		if err := owner.api.DeleteDatapack(t, dp.ID); err != nil {
			return err
		}
		deleted = true
		return nil
	}); err != nil {
		return err
	}

	var errs []error
	if kbErr != nil {
		errs = append(errs, kbErr)
	}
	check := func(name string, fn func() error) {
		if err := t.Step(name, func(*core.StepHandle) error { return fn() }); err != nil {
			errs = append(errs, err)
		}
	}
	check("owner_gets_404", func() error {
		return refused(t, "the owner's GET after delete", owner.api, get, "/datapacks/"+dp.ID, nil, statusNotFound, "NOT_FOUND")
	})
	check("member_list_drops_it", func() error {
		list, err := member.api.ListDatapacks(t)
		if err != nil {
			return err
		}
		for _, d := range list {
			if d.ID == dp.ID {
				return fmt.Errorf("the member still lists the deleted datapack %s", dp.ID)
			}
		}
		return refused(t, "the member's memories page", member.api, get, "/memories?datapack_id="+dp.ID, nil, statusNotFound, "", secret)
	})
	check("pending_invite_is_dead", func() error {
		in, err := invitee.api.IncomingShares(t)
		if err != nil {
			return err
		}
		for _, s := range in {
			if s.DatapackID == dp.ID {
				return fmt.Errorf("the invitee still sees an invite %s to the deleted datapack", s.InviteID)
			}
		}
		r, err := invitee.api.Raw(t, post, "/datapacks/invite-action", map[string]string{"invite_id": inviteID, "user_action": "accept"})
		if err != nil {
			return err
		}
		if r.Status/100 == 2 {
			return fmt.Errorf("accepting the invite to a deleted datapack answered %d: %s", r.Status, truncateBody(r.Body))
		}
		return nil
	})
	check("mcp_refuses_without_leaking", func() error {
		for _, tool := range []string{"memory_search", "knowledgebase_search", "datapack_describe"} {
			if err := toolRefused(owner, tool+" on the deleted datapack", tool, map[string]any{"datapack_id": dp.ID, "query": "marigold", "conversation_id": conv}, "marigold"); err != nil {
				return err
			}
		}
		return nil
	})
	check("project_keeps_without_the_placement", func() error {
		var detail map[string]any
		if err := owner.rest(get, "/projects/"+projectID, nil, httpOK, &detail); err != nil {
			return fmt.Errorf("the project went with the datapack: %w", err)
		}
		if strings.Contains(fmt.Sprint(detail), dp.ID) {
			return fmt.Errorf("project %s still places the deleted datapack: %s", projectID, truncate(fmt.Sprint(detail), 300))
		}
		return nil
	})
	check("no_row_names_it", func() error {
		stack := clusterEnv(t).Stack()
		var lastLeft []string
		err := eventually(t, "every table to drop the deleted datapack", visibleWithin(t), func() (bool, error) {
			var left []string
			for _, c := range cascadeTables {
				out, err := stack.YSQL(t, fmt.Sprintf("SELECT count(*) FROM %s WHERE %s::text = '%s'", c.table, c.column, dp.ID))
				if err != nil {
					return false, fmt.Errorf("count %s: %w", c.table, err)
				}
				if n, _ := strconv.Atoi(strings.TrimSpace(out)); n > 0 {
					left = append(left, fmt.Sprintf("%s=%d", c.table, n))
				}
			}
			lastLeft = left
			return len(left) == 0, nil
		})
		if err != nil {
			return fmt.Errorf("%w: rows still naming %s: %v", err, dp.ID, lastLeft)
		}
		return nil
	})
	check("name_is_free_again", func() error {
		again, err := createDatapack(t, owner.api, name)
		if err != nil {
			return fmt.Errorf("re-create %s after its delete: %w", name, err)
		}
		deleteDatapack(owner, again.ID)
		return nil
	})
	return errors.Join(errs...)
}

func waitIndexed(t *core.T, api *mekoapi.Client, datapack, key string) error {
	var last mekoapi.KBDocument
	err := t.WaitFor("document "+key+" to be indexed", indexWithin, 5*time.Second, func() (bool, error) {
		doc, err := api.KBDocumentStatus(t, datapack, key)
		if mekoapi.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		last = doc
		if doc.DocumentStatus == "FAILED" {
			return false, fmt.Errorf("document %s failed to index: %+v", key, doc)
		}
		return doc.DocumentStatus == "COMPLETED", nil
	})
	if err != nil {
		return fmt.Errorf("%w (last status %q)", err, last.DocumentStatus)
	}
	return nil
}

type projectFolder struct {
	ID            string `json:"folder_id"`
	ProjectID     string `json:"project_id"`
	Name          string `json:"folder_name"`
	DatapackCount int    `json:"datapack_count"`
}

func newProject(t *core.T, u *user, name string, folders ...string) (projectFolder, error) {
	var proj struct {
		ID string `json:"project_id"`
	}
	if err := u.rest(post, "/projects", map[string]any{"project_name": name, "folders": folders}, 201, &proj); err != nil {
		return projectFolder{}, err
	}
	var detail struct {
		Folders []projectFolder `json:"folders"`
	}
	if err := u.rest(get, "/projects/"+proj.ID, nil, httpOK, &detail); err != nil {
		return projectFolder{ProjectID: proj.ID}, err
	}
	if len(detail.Folders) == 0 {
		return projectFolder{ProjectID: proj.ID}, fmt.Errorf("project %s has no folder after its create with %v", proj.ID, folders)
	}
	f := detail.Folders[0]
	f.ProjectID = proj.ID
	return f, nil
}
