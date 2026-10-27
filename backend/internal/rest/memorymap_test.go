package rest_test

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/yugabyte/meko/backend/internal/memory"
	"github.com/yugabyte/meko/backend/internal/testenv"
)

// The memory map lays a datapack's memories out by meaning, inside [-1, 1], links near neighbors,
// and with ?q= places the query and gives each memory its similarity to it.
func TestMemoryMap(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	ctx := context.Background()
	path := "/datapacks/" + e.DatapackID + "/memory-map"
	if m := obj(okData(t, e, "GET", path, nil, 200)); len(arr(m["points"])) != 0 || m["query"] != nil {
		t.Errorf("an empty datapack's map: %v", m)
	}
	var promote string
	for _, text := range []string{"releases ship on Tuesdays", "releases need two approvals",
		"Hari owns the YCP agent", "the office is in Bangalore"} {
		a, err := e.Service.AddMemory(ctx, memory.AddMemory{DatapackID: e.DatapackID, AgentID: "claude_code:x",
			Text: text})
		if err != nil || len(a) != 1 {
			t.Fatal(a, err)
		}
		promote = a[0].ID
	}
	if _, err := e.Service.PromoteMemories(ctx, e.DatapackID, "", []string{promote}); err != nil {
		t.Fatal(err)
	}

	m := obj(okData(t, e, "GET", path+"?q=when+do+releases+ship", nil, 200))
	points := arr(m["points"])
	states := map[string]int{}
	for _, p := range points {
		p := obj(p)
		states[str(p["state"])]++
		if math.Abs(num(p["x"])) > 1.0001 || math.Abs(num(p["y"])) > 1.0001 || p["similarity"] == nil {
			t.Errorf("point %v", p)
		}
	}
	if len(points) != 4 || states["learning"] != 3 || states["collective"] != 1 || len(arr(m["query"])) != 2 {
		t.Errorf("map %v", m)
	}
	for _, l := range arr(m["links"]) {
		if a, b := int(num(obj(l)["a"])), int(num(obj(l)["b"])); a >= b || b >= len(points) {
			t.Errorf("link %v", l)
		}
	}
	// A promotion that stopped partway leaves the memory in both tables: it's drawn once, as collective.
	half, err := e.Service.AddMemory(ctx, memory.AddMemory{DatapackID: e.DatapackID, AgentID: "claude_code:x",
		Text: "promotion stopped partway"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Store.CopyToPromoted(ctx, e.DatapackID, e.UserID, half[0].ID, "a"); err != nil {
		t.Fatal(err)
	}
	// ids are drawn even when they're older than the newest the map holds; unknown ones are skipped.
	for i := range 300 {
		if _, err = e.Service.AddMemory(ctx, memory.AddMemory{DatapackID: e.DatapackID, AgentID: "claude_code:x",
			Text: fmt.Sprintf("filler memory number %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	oldest := str(obj(arr(m["points"])[0])["id"])
	m = obj(okData(t, e, "GET", path+"?ids="+oldest+",00000000-0000-0000-0000-000000000000", nil, 200))
	count := map[string]int{}
	for _, p := range arr(m["points"]) {
		count[str(obj(p)["id"])]++
		if obj(p)["id"] == half[0].ID && obj(p)["state"] != "collective" {
			t.Errorf("the half-promoted memory: %v", p)
		}
	}
	if count[half[0].ID] != 1 || count[oldest] != 1 {
		t.Errorf("half-promoted %d, asked-for oldest %d of %d points", count[half[0].ID], count[oldest],
			len(arr(m["points"])))
	}
	wantErr(t, e, "GET", "/datapacks/00000000-0000-0000-0000-000000000000/memory-map", nil, 404, "NOT_FOUND")
}
