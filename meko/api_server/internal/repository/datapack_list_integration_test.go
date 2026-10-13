package repository

// The paged and streamed datapack list against a live YugabyteDB.
//
// It needs a DISPOSABLE database named by MEKO_LIST_TEST_DSN, and skips
// without one. It applies the system migrations and deletes only the users it
// creates. It never falls back to localhost:5433, where the handler tests
// already wipe meko_system tables.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"meko/api_server/internal/db"
	"meko/api_server/internal/models"
)

const listTestOwnedCount = 2500

type listFixture struct {
	db     *sql.DB
	repo   *DatapackRepo
	owner  string
	sharer string
	// visible is every datapack owner can see, keyed by id.
	visible map[string]models.Datapack
	// hidden are datapacks shared in a way the list must not show.
	hidden []string
}

func openListTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("MEKO_LIST_TEST_DSN")
	if dsn == "" {
		t.Skip("MEKO_LIST_TEST_DSN is not set; this test needs a disposable YugabyteDB")
	}
	// Its own handle: closing the migrator closes the database it was given.
	migrateConn, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, mErr := db.RunMigrations(
		migrateConn, migrateConn, db.MigrationScope{System: true}, db.DefaultHashPartitions,
	); mErr != nil {
		t.Fatalf("migrations: %v", mErr)
	}
	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return conn
}

func listExec(t *testing.T, conn *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func listInsertUser(t *testing.T, conn *sql.DB, first string) string {
	t.Helper()
	id := uuid.NewString()
	listExec(t, conn,
		`INSERT INTO meko_system.users (user_id, user_email, user_first_name, user_last_name)
		 VALUES ($1, $2, $3, 'Tester')`, id, id+"@example.com", first)
	return id
}

func listInsertDatapack(t *testing.T, conn *sql.DB, userID, name string, created time.Time) string {
	t.Helper()
	id := uuid.NewString()
	listExec(t, conn,
		`INSERT INTO meko_system.datapacks
		   (datapack_id, user_id, created_by_agent_id, datapack_name, created_at, updated_at, universe_id,
		    datapack_details, datapack_ysql_connection_string, datapack_ysql_username, datapack_ysql_password)
		 VALUES ($1, $2, 'agent-a', $3, $4, $4, gen_random_uuid(), '{"description":"d"}', 'conn', 'u', 'p')`,
		id, userID, name, created)
	return id
}

func listShare(t *testing.T, conn *sql.DB, datapackID, owner, recipient, role, status string, agents ...string) {
	t.Helper()
	if status != "" {
		listExec(t, conn,
			`INSERT INTO meko_system.datapack_invites
			   (datapack_id, owner_user_id, recipient_email, recipient_user_id, grant_role, status)
			 VALUES ($1, $2, $3, $4, $5, $6::meko_system.invite_status)`,
			datapackID, owner, recipient+"@example.com", recipient, role, status)
	}
	for _, a := range agents {
		listExec(t, conn,
			`INSERT INTO meko_system.agent_access (user_id, agent_id, datapack_id) VALUES ($1, $2, $3)
			 ON CONFLICT DO NOTHING`, recipient, a, datapackID)
	}
}

// seed builds the owner's 2,500 datapacks, five per timestamp so the id
// tie-break matters, plus datapacks another user shares with them in every
// shape the list has to get right.
func seedListFixture(t *testing.T) *listFixture {
	conn := openListTestDB(t)
	f := &listFixture{db: conn, repo: NewDatapackRepo(conn, "", DeployEnvDev), visible: map[string]models.Datapack{}}
	f.owner = listInsertUser(t, conn, "Owner")
	f.sharer = listInsertUser(t, conn, "Sharer")
	t.Cleanup(func() {
		for _, u := range []string{f.owner, f.sharer} {
			for _, q := range []string{
				`DELETE FROM meko_system.agent_access WHERE user_id = $1`,
				`DELETE FROM meko_system.datapack_invites WHERE owner_user_id = $1`,
				`DELETE FROM meko_system.datapacks WHERE user_id = $1`,
				`DELETE FROM meko_system.users WHERE user_id = $1`,
			} {
				if _, err := conn.Exec(q, u); err != nil {
					t.Logf("cleanup %q: %v", q, err)
				}
			}
		}
		conn.Close()
	})

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// One multi-row INSERT: 2,500 single inserts take a while on RF1.
	listExec(t, conn,
		`INSERT INTO meko_system.datapacks
		   (user_id, created_by_agent_id, datapack_name, created_at, updated_at, universe_id,
		    datapack_details, datapack_ysql_connection_string, datapack_ysql_username, datapack_ysql_password)
		 SELECT $1, 'agent-a', 'dp-' || lpad(g::text, 5, '0'),
		        $2::timestamptz + ((g / 5) * interval '1 second'),
		        $2::timestamptz + ((g / 5) * interval '1 second'),
		        gen_random_uuid(), '{"description":"d"}', 'conn', 'u', 'p'
		 FROM generate_series(1, $3) g`, f.owner, base, listTestOwnedCount-3)
	// Names that need LIKE escaping.
	listInsertDatapack(t, conn, f.owner, "under_score", base.Add(10*time.Second))
	listInsertDatapack(t, conn, f.owner, "100%-done", base.Add(10*time.Second))
	listInsertDatapack(t, conn, f.owner, `back\slash`, base.Add(11*time.Second))

	tie := base.Add(20 * time.Second) // same instant as owned dp-00100..dp-00104
	sharedSpecs := []struct {
		created time.Time
		fn      func(id string)
		name    string
	}{
		{name: "shared-plain", created: base.Add(time.Hour), fn: func(id string) {
			listShare(t, conn, id, f.sharer, f.owner, "viewer", "accepted", "agent-a")
		}},
		{name: "shared-two-agents", created: base.Add(2 * time.Hour), fn: func(id string) {
			listShare(t, conn, id, f.sharer, f.owner, "contributor", "accepted", "agent-a", "agent-b")
		}},
		{name: "shared-two-invites", created: base.Add(3 * time.Hour), fn: func(id string) {
			listShare(t, conn, id, f.sharer, f.owner, "viewer", "accepted", "agent-a")
			listShare(t, conn, id, f.sharer, f.owner, "maintainer", "accepted")
		}},
		{name: "shared-tie", created: tie, fn: func(id string) {
			listShare(t, conn, id, f.sharer, f.owner, "viewer", "accepted", "agent-a")
		}},
		{name: "dp-00050", created: base.Add(30 * time.Second), fn: func(id string) { // same name as an owned one
			listShare(t, conn, id, f.sharer, f.owner, "viewer", "accepted", "agent-a")
		}},
		{name: "shared-oldest", created: base.Add(-time.Hour), fn: func(id string) {
			listShare(t, conn, id, f.sharer, f.owner, "viewer", "accepted", "agent-a")
		}},
		{name: "shared-newest", created: base.Add(48 * time.Hour), fn: func(id string) {
			listShare(t, conn, id, f.sharer, f.owner, "viewer", "accepted", "agent-a")
		}},
	}
	for _, s := range sharedSpecs {
		s.fn(listInsertDatapack(t, conn, f.sharer, s.name, s.created))
	}
	pending := listInsertDatapack(t, conn, f.sharer, "shared-pending", base)
	listShare(t, conn, pending, f.sharer, f.owner, "viewer", "pending", "agent-a")
	noInvite := listInsertDatapack(t, conn, f.sharer, "shared-no-invite", base)
	listShare(t, conn, noInvite, f.sharer, f.owner, "viewer", "", "agent-a")
	noAccess := listInsertDatapack(t, conn, f.sharer, "shared-no-access", base)
	listShare(t, conn, noAccess, f.sharer, f.owner, "viewer", "accepted")
	f.hidden = []string{pending, noInvite, noAccess}

	// The expected set, read straight from the tables.
	rows, err := conn.Query(
		`SELECT datapack_id, datapack_name, created_at, user_id FROM meko_system.datapacks
		 WHERE user_id = $1
		    OR (user_id = $2 AND (datapack_name LIKE 'shared-%' OR datapack_name = 'dp-00050'))`,
		f.owner, f.sharer)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	defer rows.Close()
	hidden := map[string]bool{pending: true, noInvite: true, noAccess: true}
	for rows.Next() {
		var d models.Datapack
		if err := rows.Scan(&d.ID, &d.Name, &d.CreatedAt, &d.UserID); err != nil {
			t.Fatal(err)
		}
		if !hidden[d.ID] {
			f.visible[d.ID] = d
		}
	}
	if want := listTestOwnedCount + len(sharedSpecs); len(f.visible) != want {
		t.Fatalf("seeded %d visible datapacks, want %d", len(f.visible), want)
	}
	return f
}

func (f *listFixture) expected(sortKey string, keep func(models.Datapack) bool) []string {
	var all []models.Datapack
	for _, d := range f.visible {
		if keep == nil || keep(d) {
			all = append(all, d)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if sortKey == DatapackSortName {
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			return a.ID < b.ID
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.ID > b.ID
	})
	ids := make([]string, len(all))
	for i, d := range all {
		ids[i] = d.ID
	}
	return ids
}

func (f *listFixture) pageAll(t *testing.T, opts DatapackListOpts) []string {
	t.Helper()
	var ids []string
	for pages := 0; ; pages++ {
		if pages > 10000 {
			t.Fatal("paging did not terminate")
		}
		page, err := f.repo.ListPage(context.Background(), f.owner, opts)
		if err != nil {
			t.Fatalf("ListPage: %v", err)
		}
		if len(page.Items) > opts.Limit {
			t.Fatalf("page has %d items, limit %d", len(page.Items), opts.Limit)
		}
		for _, d := range page.Items {
			ids = append(ids, d.ID)
		}
		if !page.HasMore {
			if page.Next != nil {
				t.Fatal("last page carries a cursor")
			}
			return ids
		}
		opts.After = page.Next
	}
}

func assertSameIDs(t *testing.T, got, want []string) {
	t.Helper()
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("datapack %s returned twice", id)
		}
		seen[id] = true
	}
	if len(got) != len(want) {
		t.Fatalf("got %d datapacks, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("position %d: got %s, want %s", i, got[i], want[i])
		}
	}
}

func TestDatapackList(t *testing.T) {
	f := seedListFixture(t)
	ctx := context.Background()

	t.Run("pages return every visible datapack once, in order", func(t *testing.T) {
		for _, sortKey := range []string{DatapackSortCreated, DatapackSortName} {
			want := f.expected(sortKey, nil)
			for _, limit := range []int{1, 7, 50, 1000} {
				got := f.pageAll(t, DatapackListOpts{Limit: limit, Sort: sortKey})
				assertSameIDs(t, got, want)
			}
		}
	})

	t.Run("filters", func(t *testing.T) {
		cases := []struct {
			keep func(models.Datapack) bool
			name string
			opts DatapackListOpts
		}{
			{name: "q substring", opts: DatapackListOpts{Q: "DP-0001"},
				keep: func(d models.Datapack) bool { return strings.Contains(d.Name, "dp-0001") }},
			{name: "q underscore is literal", opts: DatapackListOpts{Q: "_"},
				keep: func(d models.Datapack) bool { return strings.Contains(d.Name, "_") }},
			{name: "q percent is literal", opts: DatapackListOpts{Q: "%"},
				keep: func(d models.Datapack) bool { return strings.Contains(d.Name, "%") }},
			{name: "q backslash is literal", opts: DatapackListOpts{Q: `\`},
				keep: func(d models.Datapack) bool { return strings.Contains(d.Name, `\`) }},
			{name: "name exact matches owned and shared", opts: DatapackListOpts{Name: "dp-00050"},
				keep: func(d models.Datapack) bool { return d.Name == "dp-00050" }},
			{name: "grant owner", opts: DatapackListOpts{Grant: DatapackGrantOwner},
				keep: func(d models.Datapack) bool { return d.UserID == f.owner }},
			{name: "grant shared", opts: DatapackListOpts{Grant: DatapackGrantShared},
				keep: func(d models.Datapack) bool { return d.UserID != f.owner }},
			{name: "grant shared with q", opts: DatapackListOpts{Grant: DatapackGrantShared, Q: "two"},
				keep: func(d models.Datapack) bool { return d.UserID != f.owner && strings.Contains(d.Name, "two") }},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				for _, sortKey := range []string{DatapackSortCreated, DatapackSortName} {
					opts := c.opts
					opts.Sort, opts.Limit, opts.IncludeTotal = sortKey, 3, true
					want := f.expected(sortKey, c.keep)
					if len(want) == 0 {
						t.Fatal("filter matches nothing; the case tests nothing")
					}
					assertSameIDs(t, f.pageAll(t, opts), want)
					page, err := f.repo.ListPage(ctx, f.owner, opts)
					if err != nil {
						t.Fatal(err)
					}
					if page.Total == nil || *page.Total != int64(len(want)) {
						t.Fatalf("total = %v, want %d", page.Total, len(want))
					}
				}
			})
		}
	})

	t.Run("shared rows appear once with the strongest role", func(t *testing.T) {
		page, err := f.repo.ListPage(ctx, f.owner, DatapackListOpts{
			Limit: 100, Grant: DatapackGrantShared,
		})
		if err != nil {
			t.Fatal(err)
		}
		roles := map[string]string{}
		for _, d := range page.Items {
			if _, dup := roles[d.Name]; dup {
				t.Fatalf("%s listed twice", d.Name)
			}
			roles[d.Name] = d.Grant
			if d.OwnerName != "Sharer Tester" {
				t.Fatalf("%s owner name = %q", d.Name, d.OwnerName)
			}
		}
		if roles["shared-two-invites"] != "maintainer" || roles["shared-two-agents"] != "contributor" {
			t.Fatalf("roles = %v", roles)
		}
	})

	t.Run("counts match paging", func(t *testing.T) {
		owned, shared, err := f.repo.Counts(ctx, f.owner)
		if err != nil {
			t.Fatal(err)
		}
		if owned != listTestOwnedCount || shared != 7 {
			t.Fatalf("counts = %d owned, %d shared", owned, shared)
		}
	})

	t.Run("stream returns the created order and the old query's set", func(t *testing.T) {
		var got []string
		err := f.repo.StreamAll(ctx, f.owner, func(d models.Datapack) error {
			if d.UserID == f.owner && (d.Grant != "owner" || d.OwnerName != "Owner Tester") {
				t.Fatalf("owned row %s: grant %q owner %q", d.Name, d.Grant, d.OwnerName)
			}
			got = append(got, d.ID)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		assertSameIDs(t, got, f.expected(DatapackSortCreated, nil))

		// The query List ran before paging, deduplicated: same set.
		old := legacyListIDs(t, f.db, f.owner)
		sort.Strings(got)
		assertSameIDs(t, got, old)
	})

	t.Run("visible by ids", func(t *testing.T) {
		ids := append([]string{}, f.hidden...)
		want := map[string]bool{}
		for id, d := range f.visible {
			if len(want) < 5 || d.UserID != f.owner && len(want) < 12 {
				ids = append(ids, id)
				want[id] = true
			}
		}
		got, err := f.repo.VisibleByIDs(ctx, f.owner, ids)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("got %d, want %d", len(got), len(want))
		}
		for _, d := range got {
			if !want[d.ID] {
				t.Fatalf("returned %s, which was not asked for or is hidden", d.ID)
			}
		}
		shared, err := f.repo.SharedIDs(ctx, f.owner)
		if err != nil {
			t.Fatal(err)
		}
		if len(shared) != 7 {
			t.Fatalf("shared ids = %d, want 7", len(shared))
		}
	})

	t.Run("account token savings cover visible datapacks only", func(t *testing.T) {
		var owned, shared string
		for id, d := range f.visible {
			if d.UserID == f.owner && owned == "" {
				owned = id
			}
			if d.Name == "shared-plain" {
				shared = id
			}
		}
		for _, r := range []struct {
			id     string
			tokens int
		}{{owned, 100}, {shared, 40}, {f.hidden[0], 9999}} {
			listExec(t, f.db, `INSERT INTO meko_system.token_savings_summary
			    (datapack_id, source, request_count, tokens_saved_total) VALUES ($1, 'memory', 1, $2)`, r.id, r.tokens)
		}
		t.Cleanup(func() {
			if _, err := f.db.Exec(`DELETE FROM meko_system.token_savings_summary WHERE datapack_id = ANY($1::uuid[])`,
				pq.Array([]string{owned, shared, f.hidden[0]})); err != nil {
				t.Logf("cleanup: %v", err)
			}
		})
		s, err := NewTokenSavingsRepo(f.db).GetAccountSummary(ctx, f.owner, 100)
		if err != nil {
			t.Fatal(err)
		}
		if s.TotalTokensSaved != 140 || len(s.ByDatapack) != 2 || s.ByDatapack[0].DatapackID != owned {
			t.Fatalf("summary = %+v", s)
		}
	})

	t.Run("placements by project and by owner", func(t *testing.T) {
		var mine, shared string
		for id, d := range f.visible {
			if d.UserID == f.owner && mine == "" {
				mine = id
			}
			if d.Name == "shared-plain" {
				shared = id
			}
		}
		var myProject, myFolder, herProject, herFolder string
		mk := func(user, name string) (string, string) {
			var c, fo string
			if err := f.db.QueryRow(`INSERT INTO meko_system.projects (user_id, project_name, theme_key, theme_version)
			    VALUES ($1, $2, 'blank', '1') RETURNING project_id`, user, name).Scan(&c); err != nil {
				t.Fatal(err)
			}
			if err := f.db.QueryRow(`INSERT INTO meko_system.project_folders (project_id, folder_name)
			    VALUES ($1, 'f') RETURNING folder_id`, c).Scan(&fo); err != nil {
				t.Fatal(err)
			}
			return c, fo
		}
		myProject, myFolder = mk(f.owner, "mine")
		herProject, herFolder = mk(f.sharer, "hers")
		t.Cleanup(func() {
			if _, err := f.db.Exec(`DELETE FROM meko_system.projects WHERE project_id = ANY($1::uuid[])`,
				pq.Array([]string{myProject, herProject})); err != nil {
				t.Logf("cleanup: %v", err)
			}
		})
		pr := NewPlacementRepo(f.db)
		if err := pr.Place(mine, myProject, myFolder); err != nil {
			t.Fatal(err)
		}
		if err := pr.Place(shared, herProject, herFolder); err != nil {
			t.Fatal(err)
		}

		got, err := pr.OwnedByUser(f.owner, nil)
		if err != nil || len(got) != 1 || got[0].DatapackID != mine {
			t.Fatalf("OwnedByUser = %+v, %v", got, err)
		}
		got, err = pr.OwnedByUser(f.owner, []string{shared})
		if err != nil || len(got) != 0 {
			t.Fatalf("OwnedByUser filtered to a shared id = %+v, %v", got, err)
		}
		got, err = pr.InProject(herProject)
		if err != nil || len(got) != 1 || got[0].DatapackID != shared {
			t.Fatalf("InProject = %+v, %v", got, err)
		}
	})

	t.Run("paging under concurrent writes", func(t *testing.T) {
		stable := f.expected(DatapackSortCreated, nil)
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				// Newest and oldest, so writes land ahead of and behind the cursor.
				ts := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
				if i%2 == 1 {
					ts = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
				}
				id := listInsertDatapack(t, f.db, f.owner, fmt.Sprintf("churn-%d", i), ts)
				if i%3 == 0 {
					listExec(t, f.db, `DELETE FROM meko_system.datapacks WHERE datapack_id = $1`, id)
				}
			}
		}()
		got := f.pageAll(t, DatapackListOpts{Limit: 50, Sort: DatapackSortCreated})
		close(stop)
		wg.Wait()
		listExec(t, f.db, `DELETE FROM meko_system.datapacks WHERE user_id = $1 AND datapack_name LIKE 'churn-%'`, f.owner)

		seen := map[string]int{}
		for _, id := range got {
			seen[id]++
		}
		for id, n := range seen {
			if n > 1 {
				t.Fatalf("datapack %s returned %d times", id, n)
			}
		}
		for _, id := range stable {
			if seen[id] != 1 {
				t.Fatalf("datapack %s, present for the whole run, returned %d times", id, seen[id])
			}
		}
	})

	t.Run("keyset pages read about limit rows", func(t *testing.T) {
		// Deep in the account, both sorts, the exact SQL ListPage runs.
		scanned := regexp.MustCompile(`Storage (?:Table|Index) Rows Scanned: (\d+)`)
		for _, sortKey := range []string{DatapackSortCreated, DatapackSortName} {
			mid := f.visible[f.expected(sortKey, func(d models.Datapack) bool { return d.UserID == f.owner })[1200]]
			key := &DatapackCursorKey{ID: mid.ID, CreatedAt: mid.CreatedAt, Name: mid.Name}
			q, args := ownedPageQuery(f.owner, DatapackListOpts{Sort: sortKey, After: key}, 51)
			rows, err := f.db.Query(`EXPLAIN (ANALYZE, DIST, COSTS OFF) `+q, args...)
			if err != nil {
				t.Fatal(err)
			}
			var plan []string
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, line)
			}
			rows.Close()
			joined := strings.Join(plan, "\n")
			t.Logf("owned page, sort=%s, 1200 rows deep:\n%s", sortKey, joined)
			if strings.Contains(joined, "Sort Key") {
				t.Fatalf("sort=%s sorts instead of reading an index in order", sortKey)
			}
			for _, m := range scanned.FindAllStringSubmatch(joined, -1) {
				if n, aErr := strconv.Atoi(m[1]); aErr != nil || n > 60 {
					t.Fatalf("sort=%s scanned %d rows for a 51-row page", sortKey, n)
				}
			}
		}
	})
}

// legacyListIDs runs the query List used before paging, deduplicated and sorted.
func legacyListIDs(t *testing.T, conn *sql.DB, userID string) []string {
	t.Helper()
	rows, err := conn.Query(
		`SELECT DISTINCT id FROM (
		   SELECT d.datapack_id::text AS id FROM meko_system.datapacks d
		   JOIN meko_system.users u ON u.user_id = d.user_id
		   WHERE d.user_id = $1
		   UNION ALL
		   SELECT d.datapack_id::text FROM meko_system.datapacks d
		   JOIN meko_system.users u ON u.user_id = d.user_id
		   JOIN meko_system.agent_access aa ON aa.datapack_id = d.datapack_id AND aa.user_id = $1
		   JOIN meko_system.datapack_invites i ON i.datapack_id = d.datapack_id
		        AND i.recipient_user_id = $1 AND i.status = 'accepted'
		   WHERE d.user_id != $1) x ORDER BY id`, userID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}
