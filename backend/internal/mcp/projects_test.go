package mcp_test

import (
	"testing"

	"github.com/yugabyte/meko/backend/internal/testenv"
)

// The project tools, as Meko Cloud's: a project starts with the folders named (one at least),
// a datapack is filed in one folder and moved by filing it again, datapack_create files a new one in
// a folder it checks first, datapack_describe says where one is filed (nulls when Unfiled), and
// deleting a folder or a project unfiles without deleting.
func TestProjectTools(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	with := func(kv ...any) map[string]any {
		m := map[string]any{"conversation_id": "c"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[str(kv[i])] = kv[i+1]
		}
		return m
	}
	wantError(t, e, "project_list", map[string]any{}, "conversation_id_required")
	wantError(t, e, "project_create", with(), "project_name_required")
	wantError(t, e, "project_create", with("project_name", "web"), "VALIDATION_ERROR") // no folder
	p := ok(t, e, "project_create", with("project_name", "web", "description", "the site",
		"folders", []string{"design", " ", "tests"})).JSON
	id := str(p["project_id"])
	if p["project_name"] != "web" || num(p["folder_count"]) != 2 {
		t.Fatalf("create: %v", p)
	}
	wantError(t, e, "project_create", with("project_name", "web", "folders", []string{"a1"}), "CONFLICT")
	if l := ok(t, e, "project_list", with()).JSON; len(arr(l["owned"])) != 1 {
		t.Errorf("list: %v", l)
	}

	f := ok(t, e, "project_folder_create", with("project_id", id, "folder_name", "docs")).JSON
	folder := str(f["folder_id"])
	wantError(t, e, "project_folder_update", with("project_id", id, "folder_id", folder), "nothing_to_update")
	ok(t, e, "project_folder_update", with("project_id", id, "folder_id", folder, "description", "what we wrote"))
	ok(t, e, "project_update", with("project_id", id, "description", ""))
	wantError(t, e, "project_describe", with("project_id", "nope"), "NOT_FOUND")

	unfiled := ok(t, e, "datapack_describe", with("datapack_id", e.DatapackID)).JSON
	if v, has := unfiled["path"]; !has || v != nil {
		t.Errorf("an Unfiled datapack's path: %v", unfiled)
	}
	ok(t, e, "project_add_datapack", with("datapack_id", e.DatapackID, "folder_id", folder))
	if d := ok(t, e, "datapack_describe", with("datapack_id", e.DatapackID)).JSON; d["path"] != "web/docs" {
		t.Errorf("filed: %v", d)
	}
	wantError(t, e, "datapack_create", with("name", "nowhere", "folder_id", "nope"), "NOT_FOUND")
	if l := ok(t, e, "datapack_list", with()); len(l.List) != 1 {
		t.Errorf("a wrong folder made a datapack: %s", l.Text)
	}
	ok(t, e, "datapack_create", with("name", "specs", "folder_id", folder))
	d := ok(t, e, "project_describe", with("project_id", id)).JSON
	if obj(d["project"])["description"] != "" || len(arr(d["folders"])) != 3 || len(arr(d["datapacks"])) != 2 {
		t.Errorf("describe: %v", d)
	}
	if s := ok(t, e, "project_stats", with("project_id", id)).JSON; s["kb_files"] == nil {
		t.Errorf("stats: %v", s)
	}

	ok(t, e, "project_remove_datapack", with("datapack_id", e.DatapackID))
	ok(t, e, "project_folder_delete", with("project_id", id, "folder_id", folder))
	ok(t, e, "project_delete", with("project_id", id))
	if l := ok(t, e, "datapack_list", with()); len(l.List) != 2 {
		t.Errorf("deleting the project deleted a datapack: %s", l.Text)
	}
}
