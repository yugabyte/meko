package rest_test

import (
	"testing"

	"github.com/yugabyte/meko/src/meko/internal/testenv"
)

// Projects, as Meko Cloud's: a project starts from a layout's folders (or the ones named), names
// are checked and unique, a datapack is filed in one folder at a time, and deleting a folder or a
// project unfiles its datapacks without deleting them. The activity log has the folders made and the
// datapacks filed.
func TestProjects(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	if th := arr(okData(t, e, "GET", "/projects/themes", nil, 200)); len(th) != 3 {
		t.Fatalf("themes %v", th)
	}
	wantErr(t, e, "POST", "/projects", map[string]any{"project_name": "x"}, 400, "VALIDATION_ERROR")
	wantErr(t, e, "POST", "/projects", map[string]any{"project_name": "a/b", "theme_key": "adlc"}, 400,
		"VALIDATION_ERROR")
	wantErr(t, e, "POST", "/projects", map[string]any{"project_name": "web"}, 400, "VALIDATION_ERROR") // custom: no folder
	wantErr(t, e, "POST", "/projects", map[string]any{"project_name": "web", "theme_key": "nope"}, 400,
		"VALIDATION_ERROR")

	p := obj(okData(t, e, "POST", "/projects", map[string]any{"project_name": " web ", "description": "the site",
		"theme_key": "adlc"}, 201))
	id := str(p["project_id"])
	if p["project_name"] != "web" || num(p["folder_count"]) != 3 || p["description"] != "the site" {
		t.Fatalf("created %v", p)
	}
	wantErr(t, e, "POST", "/projects", map[string]any{"project_name": "web", "folders": []string{"a1"}}, 409,
		"CONFLICT")
	okData(t, e, "POST", "/projects", map[string]any{"project_name": "ops", "folders": []string{"runbooks"}}, 201)
	if l := obj(okData(t, e, "GET", "/projects", nil, 200)); len(arr(l["owned"])) != 2 || len(arr(l["shared"])) != 0 {
		t.Fatalf("list %v", l)
	}

	f := obj(okData(t, e, "POST", "/projects/"+id+"/folders", map[string]any{"folder_name": "docs",
		"folder_description": "what we wrote"}, 201))
	folder := str(f["folder_id"])
	wantErr(t, e, "POST", "/projects/"+id+"/folders", map[string]any{"folder_name": "docs"}, 409, "CONFLICT")
	okData(t, e, "PATCH", "/projects/"+id+"/folders/"+folder, map[string]any{"folder_name": "design"}, 200)
	wantErr(t, e, "PATCH", "/projects/"+id+"/folders/"+folder, map[string]any{}, 400, "VALIDATION_ERROR")
	okData(t, e, "PATCH", "/projects/"+id, map[string]any{"description": "the web site"}, 200)
	wantErr(t, e, "PATCH", "/projects/"+id, map[string]any{"project_name": "ops"}, 409, "CONFLICT")

	okData(t, e, "POST", "/datapacks/"+e.DatapackID+"/placement", map[string]any{"folder_id": folder}, 200)
	wantErr(t, e, "POST", "/datapacks/"+e.DatapackID+"/placement", map[string]any{"folder_id": "nope"}, 404,
		"NOT_FOUND")
	wantErr(t, e, "POST", "/datapacks/00000000-0000-0000-0000-000000000000/placement",
		map[string]any{"folder_id": folder}, 404, "NOT_FOUND")
	made := obj(okData(t, e, "POST", "/datapacks", map[string]any{"name": "specs", "folder_id": folder}, 201))
	if p := obj(okData(t, e, "GET", "/projects/placements?datapack_ids="+str(made["datapack_id"]), nil,
		200)); obj(p[str(made["datapack_id"])])["folder_id"] != folder {
		t.Errorf("a create in a folder: %v", p)
	}
	wantErr(t, e, "POST", "/datapacks", map[string]any{"name": "nowhere", "folder_id": "nope"}, 404, "NOT_FOUND")
	for _, d := range arr(okData(t, e, "GET", "/datapacks", nil, 200)) {
		if obj(d)["datapack_name"] == "nowhere" {
			t.Errorf("a wrong folder made a datapack")
		}
	}
	wantErr(t, e, "GET", "/projects/placements?datapack_ids=nope", nil, 400, "INVALID_DATAPACK_IDS")
	pl := obj(obj(okData(t, e, "GET", "/projects/placements?datapack_ids="+e.DatapackID, nil, 200))[e.DatapackID])
	if pl["path"] != "web/design" || pl["folder_id"] != folder {
		t.Fatalf("placement %v", pl)
	}

	d := obj(okData(t, e, "GET", "/projects/"+id, nil, 200))
	if obj(d["project"])["description"] != "the web site" || len(arr(d["folders"])) != 4 ||
		len(arr(d["datapacks"])) != 2 || obj(arr(d["datapacks"])[0])["folder_id"] != folder {
		t.Fatalf("detail %v", d)
	}
	types := map[string]int{}
	for _, it := range arr(obj(okData(t, e, "GET", "/projects/"+id+"/activity-log", nil, 200))["items"]) {
		types[str(obj(it)["type"])]++
	}
	if types["folder_created"] != 4 || types["datapack_filed"] != 2 {
		t.Errorf("activity %v", types)
	}
	page := obj(okData(t, e, "GET", "/projects/"+id+"/activity-log?limit=2", nil, 200))
	next := obj(okData(t, e, "GET", "/projects/"+id+"/activity-log?limit=2&cursor="+str(page["next_cursor"]),
		nil, 200))
	if page["has_more"] != true || len(arr(next["items"])) != 2 {
		t.Errorf("paging %v then %v", page, next)
	}

	okData(t, e, "DELETE", "/projects/"+id+"/folders/"+folder, nil, 200)
	if pl := obj(okData(t, e, "GET", "/projects/placements", nil, 200)); len(pl) != 0 {
		t.Errorf("still filed after its folder went: %v", pl)
	}
	okData(t, e, "GET", "/datapacks/"+e.DatapackID, nil, 200)
	first := obj(arr(d["folders"])[0])["folder_id"]
	okData(t, e, "POST", "/datapacks/"+e.DatapackID+"/placement", map[string]any{"folder_id": first}, 200)
	okData(t, e, "DELETE", "/projects/"+id, nil, 200)
	wantErr(t, e, "GET", "/projects/"+id, nil, 404, "NOT_FOUND")
	okData(t, e, "GET", "/datapacks/"+e.DatapackID, nil, 200)
	if pl := obj(okData(t, e, "GET", "/projects/placements", nil, 200)); len(pl) != 0 {
		t.Errorf("still filed after its project went: %v", pl)
	}
}
