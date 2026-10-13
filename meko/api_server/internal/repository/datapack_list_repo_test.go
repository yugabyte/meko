package repository

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"meko/api_server/internal/models"
)

const cursorTestID = "3e5b4c20-ed05-4c69-ad5c-9a4da3b5b9eb"

func TestDatapackCursor_RoundTrip(t *testing.T) {
	h := DatapackFilterHash("q", "", "owner")
	at := time.Date(2026, 9, 28, 22, 8, 41, 441308000, time.UTC)

	got, err := DecodeDatapackCursor(EncodeDatapackCursor(DatapackSortCreated, h,
		DatapackCursorKey{CreatedAt: at, ID: cursorTestID}), DatapackSortCreated, h)
	if err != nil || !got.CreatedAt.Equal(at) || got.ID != cursorTestID {
		t.Fatalf("created: got %+v, %v", got, err)
	}

	got, err = DecodeDatapackCursor(EncodeDatapackCursor(DatapackSortName, h,
		DatapackCursorKey{Name: "a/b c", ID: cursorTestID}), DatapackSortName, h)
	if err != nil || got.Name != "a/b c" || got.ID != cursorTestID {
		t.Fatalf("name: got %+v, %v", got, err)
	}

	upper := strings.ToUpper(cursorTestID)
	raw := base64.RawURLEncoding.EncodeToString([]byte(
		`{"v":1,"s":"name","k":["M","` + upper + `"],"f":"` + h + `"}`))
	got, err = DecodeDatapackCursor(raw, DatapackSortName, h)
	if err != nil || got.ID != cursorTestID || got.Name != "M" {
		t.Fatalf("uppercase id: got %+v, %v", got, err)
	}
}

func TestDatapackCursor_RejectsRatherThanRestarting(t *testing.T) {
	h := DatapackFilterHash("", "", "")
	good := EncodeDatapackCursor(DatapackSortCreated, h, DatapackCursorKey{CreatedAt: time.Now(), ID: cursorTestID})
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

	raw := func(v, ts, id string) string {
		return enc(`{"v":` + v + `,"s":"created","k":["` + ts + `","` + id + `"],"f":"` + h + `"}`)
	}
	for name, c := range map[string]struct{ cursor, sort, hash string }{
		"not base64":      {"%%%", DatapackSortCreated, h},
		"not json":        {enc("nope"), DatapackSortCreated, h},
		"tampered":        {good[:len(good)-4] + "AAAA", DatapackSortCreated, h},
		"wrong version":   {raw("2", "2026-01-01T00:00:00Z", cursorTestID), DatapackSortCreated, h},
		"sort mismatch":   {good, DatapackSortName, h},
		"filter mismatch": {good, DatapackSortCreated, DatapackFilterHash("x", "", "")},
		"bad timestamp":   {raw("1", "yesterday", cursorTestID), DatapackSortCreated, h},
		"bad id":          {raw("1", "2026-01-01T00:00:00Z", "x' OR 1=1"), DatapackSortCreated, h},
	} {
		if _, err := DecodeDatapackCursor(c.cursor, c.sort, c.hash); !errors.Is(err, ErrInvalidDatapackCursor) {
			t.Errorf("%s: err = %v, want ErrInvalidDatapackCursor", name, err)
		}
	}
}

func TestDatapackFilterHash_DistinguishesFields(t *testing.T) {
	// "ab"+"" and "a"+"b" must not collide.
	if DatapackFilterHash("ab", "", "") == DatapackFilterHash("a", "b", "") {
		t.Fatal("filter hash ignores field boundaries")
	}
}

func TestEscapeLike(t *testing.T) {
	if got := escapeLike(`50%_off\now`); got != `50\%\_off\\now` {
		t.Fatalf("escapeLike = %q", got)
	}
}

var pageCols = []string{
	"datapack_id", "user_id", "datapack_name", "created_by_agent_id", "universe_id",
	"conn", "mcp_user", "created_at", "updated_at", "description",
	"owner_name", "owner_email", "grant",
}

// The page is the merge of two branches that each returned their first
// limit+1 rows. Interleaved and equal timestamps must come out in
// (created_at DESC, id DESC) order, and the cursor is the last row returned.
func TestListPage_MergesBranchesInOrder(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := NewDatapackRepo(db, "", DeployEnvProd)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	owned := sqlmock.NewRows(pageCols)
	for _, r := range []struct {
		at time.Time
		id string
	}{
		{t0.Add(5 * time.Second), "aaaaaaaa-0000-0000-0000-000000000009"},
		{t0.Add(3 * time.Second), "aaaaaaaa-0000-0000-0000-000000000005"},
		{t0.Add(3 * time.Second), "aaaaaaaa-0000-0000-0000-000000000001"},
	} {
		owned.AddRow(r.id, "me", "o", "", "u", "", "", r.at, r.at, "", "", "", "owner")
	}
	shared := sqlmock.NewRows(pageCols)
	for _, r := range []struct {
		at time.Time
		id string
	}{
		{t0.Add(4 * time.Second), "bbbbbbbb-0000-0000-0000-000000000000"},
		{t0.Add(3 * time.Second), "aaaaaaaa-0000-0000-0000-000000000003"}, // ties owned on time
		{t0.Add(time.Second), "bbbbbbbb-0000-0000-0000-000000000001"},
	} {
		shared.AddRow(r.id, "other", "s", "", "u", "", "", r.at, r.at, "", "Other", "", "viewer")
	}
	mock.ExpectQuery(`FROM meko_system.datapacks d\s+WHERE d.user_id = \$1`).
		WithArgs("me", 5).WillReturnRows(owned)
	mock.ExpectQuery("FROM meko_system.users WHERE user_id").
		WillReturnRows(sqlmock.NewRows([]string{"n", "e"}).AddRow("Me", "me@example.com"))
	mock.ExpectQuery("WITH shared AS").WithArgs("me", 5).WillReturnRows(shared)

	page, err := repo.ListPage(context.Background(), "me", DatapackListOpts{Limit: 4, Sort: DatapackSortCreated})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"aaaaaaaa-0000-0000-0000-000000000009",
		"bbbbbbbb-0000-0000-0000-000000000000",
		"aaaaaaaa-0000-0000-0000-000000000005",
		"aaaaaaaa-0000-0000-0000-000000000003",
	}
	if len(page.Items) != len(want) || !page.HasMore {
		t.Fatalf("page = %+v", page.Items)
	}
	for i, id := range want {
		if page.Items[i].ID != id {
			t.Fatalf("position %d = %s, want %s", i, page.Items[i].ID, id)
		}
	}
	if page.Next == nil || page.Next.ID != want[3] || !page.Next.CreatedAt.Equal(t0.Add(3*time.Second)) {
		t.Fatalf("cursor = %+v", page.Next)
	}
	if page.Items[0].OwnerName != "Me" || page.Items[1].OwnerName != "Other" {
		t.Fatalf("owner names = %q, %q", page.Items[0].OwnerName, page.Items[1].OwnerName)
	}
}

// Equal timestamps across the branches order by id descending.
func TestDatapackBefore_TieBreaksOnID(t *testing.T) {
	at := time.Now()
	a := models.Datapack{ID: "aaaaaaaa-0000-0000-0000-000000000005", Name: "x", CreatedAt: at}
	b := models.Datapack{ID: "aaaaaaaa-0000-0000-0000-000000000003", Name: "x", CreatedAt: at}
	if !datapackBefore(DatapackSortCreated, &a, &b) || datapackBefore(DatapackSortCreated, &b, &a) {
		t.Fatal("created order must break ties by id descending")
	}
	if datapackBefore(DatapackSortName, &a, &b) || !datapackBefore(DatapackSortName, &b, &a) {
		t.Fatal("name order must break ties by id ascending")
	}
}

// Name order is pinned to C collation so it matches datapackBefore on a
// database whose collation is not C.
func TestNameSortUsesCCollation(t *testing.T) {
	key := &DatapackCursorKey{Name: "M", ID: cursorTestID}
	owned, _ := ownedPageQuery("me", DatapackListOpts{Sort: DatapackSortName, After: key}, 10)
	shared, _ := sharedListQuery("me", DatapackListOpts{Sort: DatapackSortName, After: key}, 10)
	for _, q := range []string{owned, shared, orderBy(DatapackSortName, true), orderBy(DatapackSortName, false)} {
		if !strings.Contains(q, `COLLATE "C"`) {
			t.Fatalf("name SQL is not pinned to C collation:\n%s", q)
		}
	}
	created, _ := ownedPageQuery("me", DatapackListOpts{
		Sort:  DatapackSortCreated,
		After: &DatapackCursorKey{ID: cursorTestID, CreatedAt: time.Now()},
	}, 10)
	if strings.Contains(created, "COLLATE") {
		t.Fatalf("created SQL sets a collation:\n%s", created)
	}
}
