package repository

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"

	"meko/api_server/internal/models"
)

// Sort orders for DatapackRepo.ListPage.
const (
	DatapackSortCreated = "created" // created_at DESC, datapack_id DESC
	DatapackSortName    = "name"    // datapack_name COLLATE "C" ASC, datapack_id ASC
)

// cName is datapack_name in C collation: bytewise, the order datapackBefore
// merges. The column's own collation is whatever the database was created
// with, and under anything but C the SQL bound and the Go merge disagree, so
// a name-sorted page silently drops rows. On a C database this expression
// matches uq_datapacks_user_name, so the bound is still an index range.
const cName = `d.datapack_name COLLATE "C"`

// Grant filters for DatapackRepo.ListPage. Empty means both.
const (
	DatapackGrantOwner  = "owner"
	DatapackGrantShared = "shared"
)

// DatapackCursorKey is the sort key of the last row a page returned. Only the
// fields of the page's sort are set: CreatedAt for "created", Name for "name".
type DatapackCursorKey struct {
	CreatedAt time.Time
	Name      string
	ID        string
}

// DatapackListOpts selects one page of the datapacks a user can see.
type DatapackListOpts struct {
	After        *DatapackCursorKey
	Sort         string
	Q            string
	Name         string
	Grant        string
	Limit        int
	IncludeTotal bool
}

// DatapackPage is one page of ListPage. Next is nil when HasMore is false.
type DatapackPage struct {
	Next    *DatapackCursorKey
	Total   *int64
	Items   []models.Datapack
	HasMore bool
}

// datapackColumns are the columns every list query scans, in scanDatapackRow's
// order. The owner's name and email come from meko_system.users u.
const datapackColumns = `d.datapack_id, d.user_id, d.datapack_name,
		        COALESCE(d.created_by_agent_id, ''),
		        d.universe_id, d.datapack_ysql_connection_string,
		        d.datapack_ysql_username,
		        d.created_at, d.updated_at,
		        COALESCE(d.datapack_details->>'description', ''),
		        COALESCE(CONCAT(u.user_first_name, ' ', u.user_last_name), ''),
		        COALESCE(u.user_email, '')`

// sharedInvitesCTE is the one definition of "shared with $1" the list uses: an
// accepted invite AND an agent_access row, collapsed to one row per datapack.
// A recipient with several accepted invites, or access under several agents,
// used to see the datapack once per combination. When the invites disagree on
// the role, the strongest one wins.
//
// It starts from idx_invites_recipient_status rather than agent_access, whose
// primary key needs the agent_id too.
//
// Access checks (GetByIDForSharedAccess) accept agent_access without an
// invite; the list keeps requiring the invite so it shows no new rows.
const sharedInvitesCTE = `shared AS (
		SELECT i.datapack_id,
		       (ARRAY_AGG(i.grant_role ORDER BY CASE i.grant_role
		            WHEN 'maintainer' THEN 3 WHEN 'contributor' THEN 2 WHEN 'viewer' THEN 1 ELSE 0
		        END DESC))[1] AS grant_role
		FROM meko_system.datapack_invites i
		WHERE i.recipient_user_id = $1 AND i.status = 'accepted'
		  AND EXISTS (SELECT 1 FROM meko_system.agent_access aa
		              WHERE aa.datapack_id = i.datapack_id AND aa.user_id = $1)
		GROUP BY i.datapack_id
	)`

func scanDatapackRow(s rowScanner, d *models.Datapack) error {
	return s.Scan(
		&d.ID, &d.UserID, &d.Name,
		&d.CreatedByAgentID,
		&d.UniverseID, &d.YBUniverseConn,
		&d.MCPUserName,
		&d.CreatedAt, &d.UpdatedAt,
		&d.Description,
		&d.OwnerName,
		&d.OwnerEmail,
		&d.Grant,
	)
}

// datapackFilter builds the name filters and keyset predicate shared by both
// branches. Arguments are appended after $1 (the user id).
type datapackFilter struct {
	where []string
	args  []any
}

func (f *datapackFilter) add(clause string, vals ...any) {
	for _, v := range vals {
		f.args = append(f.args, v)
		clause = strings.Replace(clause, "?", fmt.Sprintf("$%d", len(f.args)+1), 1)
	}
	f.where = append(f.where, clause)
}

func (f *datapackFilter) sql() string {
	if len(f.where) == 0 {
		return ""
	}
	return " AND " + strings.Join(f.where, " AND ")
}

// escapeLike makes q match literally inside an ILIKE pattern.
func escapeLike(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(q)
}

func nameFilters(opts DatapackListOpts) datapackFilter {
	var f datapackFilter
	if opts.Q != "" {
		f.add(`d.datapack_name ILIKE ? ESCAPE '\'`, "%"+escapeLike(opts.Q)+"%")
	}
	if opts.Name != "" {
		f.add(`d.datapack_name = ?`, opts.Name)
	}
	return f
}

// keysetFilter adds the "after the cursor" predicate. The leading >= / <= on
// the first sort column is what YugabyteDB turns into an index range bound;
// the OR alone, or a row comparison, is applied as a filter over every row
// from the start of the owner's range, so a deep page would read the whole
// account up to the cursor.
//
// The owned branch in name order gets the bound alone. uq_datapacks_user_name
// does not carry datapack_id, so the tie-break would be checked only after the
// table fetch, which reads a whole prefetch batch before LIMIT applies.
// Names are unique per owner, so at most one owned row ties the cursor's
// name, and ownedPage drops it in Go.
func keysetFilter(f *datapackFilter, opts DatapackListOpts, owned bool) {
	k := opts.After
	if k == nil {
		return
	}
	if opts.Sort == DatapackSortName {
		if owned {
			f.add(cName+` >= ?`, k.Name)
			return
		}
		f.add(cName+` >= ? AND (`+cName+` > ? OR d.datapack_id > ?::uuid)`, k.Name, k.Name, k.ID)
		return
	}
	f.add(`d.created_at <= ? AND (d.created_at < ? OR d.datapack_id < ?::uuid)`, k.CreatedAt, k.CreatedAt, k.ID)
}

func orderBy(sortKey string, owned bool) string {
	if sortKey == DatapackSortName {
		if owned {
			return "ORDER BY " + cName + " ASC"
		}
		return "ORDER BY " + cName + " ASC, d.datapack_id ASC"
	}
	return "ORDER BY d.created_at DESC, d.datapack_id DESC"
}

// ownerOf returns the owner's display name and email, which the owned branch
// would otherwise join once per row.
func (r *DatapackRepo) ownerOf(ctx context.Context, userID string) (name, email string, err error) {
	err = r.db.QueryRowContext(ctx,
		`SELECT COALESCE(CONCAT(user_first_name, ' ', user_last_name), ''), COALESCE(user_email, '')
		 FROM meko_system.users WHERE user_id = $1`,
		userID,
	).Scan(&name, &email)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("failed to read datapack owner: %w", err)
	}
	return name, email, nil
}

// ownedColumns is datapackColumns without the users join: the owner is
// filled in from ownerOf.
const ownedColumns = `d.datapack_id, d.user_id, d.datapack_name,
		        COALESCE(d.created_by_agent_id, ''),
		        d.universe_id, d.datapack_ysql_connection_string,
		        d.datapack_ysql_username,
		        d.created_at, d.updated_at,
		        COALESCE(d.datapack_details->>'description', ''),
		        '', '', 'owner'`

func (r *DatapackRepo) queryDatapacks(ctx context.Context, query string, args []any) ([]models.Datapack, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list datapacks: %w", err)
	}
	defer rows.Close()
	var out []models.Datapack
	for rows.Next() {
		var d models.Datapack
		if err := scanDatapackRow(rows, &d); err != nil {
			return nil, fmt.Errorf("failed to scan datapack: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	return out, nil
}

// ownedPageQuery is the owned branch of one page: at most n rows after the
// cursor, read in index order.
func ownedPageQuery(userID string, opts DatapackListOpts, n int) (string, []any) {
	f := nameFilters(opts)
	keysetFilter(&f, opts, true)
	if opts.Sort == DatapackSortName && opts.After != nil {
		n++ // room for the tie row ownedPage drops
	}
	args := append([]any{userID}, f.args...)
	args = append(args, n)
	q := fmt.Sprintf(`SELECT %s
		 FROM meko_system.datapacks d
		 WHERE d.user_id = $1%s
		 %s
		 LIMIT $%d`, ownedColumns, f.sql(), orderBy(opts.Sort, true), len(args))
	return q, args
}

func (r *DatapackRepo) ownedPage(
	ctx context.Context, userID string, opts DatapackListOpts, n int,
) ([]models.Datapack, error) {
	q, args := ownedPageQuery(userID, opts, n)
	items, err := r.queryDatapacks(ctx, q, args)
	if err != nil {
		return nil, err
	}
	if k := opts.After; opts.Sort == DatapackSortName && k != nil {
		kept := items[:0]
		for _, d := range items {
			if d.Name != k.Name || d.ID > k.ID {
				kept = append(kept, d)
			}
		}
		items = kept
		if len(items) > n {
			items = items[:n]
		}
	}
	if len(items) == 0 {
		return items, nil
	}
	name, email, err := r.ownerOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].OwnerName, items[i].OwnerEmail = name, email
	}
	return items, nil
}

func sharedListQuery(userID string, opts DatapackListOpts, n int) (string, []any) {
	f := nameFilters(opts)
	keysetFilter(&f, opts, false)
	args := append([]any{userID}, f.args...)
	limit := ""
	if n > 0 {
		args = append(args, n)
		limit = fmt.Sprintf("LIMIT $%d", len(args))
	}
	q := fmt.Sprintf(`WITH %s
		 SELECT %s, s.grant_role
		 FROM shared s
		 JOIN meko_system.datapacks d ON d.datapack_id = s.datapack_id
		 JOIN meko_system.users u ON u.user_id = d.user_id
		 WHERE d.user_id <> $1%s
		 %s
		 %s`, sharedInvitesCTE, datapackColumns, f.sql(), orderBy(opts.Sort, false), limit)
	return q, args
}

func (r *DatapackRepo) sharedPage(
	ctx context.Context, userID string, opts DatapackListOpts, n int,
) ([]models.Datapack, error) {
	q, args := sharedListQuery(userID, opts, n)
	return r.queryDatapacks(ctx, q, args)
}

// datapackBefore reports whether a sorts before b in the page order.
// Names compare bytewise, which is what COLLATE "C" produces in SQL.
func datapackBefore(sortKey string, a, b *models.Datapack) bool {
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
}

// ListPage returns one page of the datapacks userID can see, owned and shared
// merged into one order. Each branch reads at most Limit+1 rows after the
// cursor in that same order, so their merge holds the true first Limit+1.
func (r *DatapackRepo) ListPage(ctx context.Context, userID string, opts DatapackListOpts) (*DatapackPage, error) {
	if opts.Limit <= 0 {
		return nil, fmt.Errorf("limit must be positive")
	}
	if opts.Sort == "" {
		opts.Sort = DatapackSortCreated
	}
	n := opts.Limit + 1

	var merged []models.Datapack
	if opts.Grant != DatapackGrantShared {
		owned, err := r.ownedPage(ctx, userID, opts, n)
		if err != nil {
			return nil, err
		}
		merged = append(merged, owned...)
	}
	if opts.Grant != DatapackGrantOwner {
		shared, err := r.sharedPage(ctx, userID, opts, n)
		if err != nil {
			return nil, err
		}
		merged = append(merged, shared...)
	}
	sort.SliceStable(merged, func(i, j int) bool { return datapackBefore(opts.Sort, &merged[i], &merged[j]) })

	page := &DatapackPage{Items: merged}
	if len(merged) > opts.Limit {
		page.Items = merged[:opts.Limit]
		page.HasMore = true
		last := page.Items[len(page.Items)-1]
		page.Next = &DatapackCursorKey{ID: last.ID}
		if opts.Sort == DatapackSortName {
			page.Next.Name = last.Name
		} else {
			page.Next.CreatedAt = last.CreatedAt
		}
	}
	if page.Items == nil {
		page.Items = []models.Datapack{}
	}

	if opts.IncludeTotal {
		total, err := r.countMatching(ctx, userID, opts)
		if err != nil {
			return nil, err
		}
		page.Total = &total
	}
	return page, nil
}

// countMatching counts the rows ListPage would return across all pages.
func (r *DatapackRepo) countMatching(ctx context.Context, userID string, opts DatapackListOpts) (int64, error) {
	var total int64
	f := nameFilters(opts)
	args := append([]any{userID}, f.args...)
	if opts.Grant != DatapackGrantShared {
		var n int64
		if err := r.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM meko_system.datapacks d WHERE d.user_id = $1`+f.sql(), args...,
		).Scan(&n); err != nil {
			return 0, fmt.Errorf("count owned datapacks: %w", err)
		}
		total += n
	}
	if opts.Grant != DatapackGrantOwner {
		var n int64
		if err := r.db.QueryRowContext(ctx,
			`WITH `+sharedInvitesCTE+`
			 SELECT COUNT(*) FROM shared s
			 JOIN meko_system.datapacks d ON d.datapack_id = s.datapack_id
			 WHERE d.user_id <> $1`+f.sql(), args...,
		).Scan(&n); err != nil {
			return 0, fmt.Errorf("count shared datapacks: %w", err)
		}
		total += n
	}
	return total, nil
}

// Counts returns how many datapacks userID owns and how many are shared with
// them, by the same definitions as the list.
func (r *DatapackRepo) Counts(ctx context.Context, userID string) (owned, shared int64, err error) {
	owned, err = r.countMatching(ctx, userID, DatapackListOpts{Grant: DatapackGrantOwner})
	if err != nil {
		return 0, 0, err
	}
	shared, err = r.countMatching(ctx, userID, DatapackListOpts{Grant: DatapackGrantShared})
	if err != nil {
		return 0, 0, err
	}
	return owned, shared, nil
}

// nextDatapack scans the next list row. ok is false when the result is exhausted.
func nextDatapack(rows *sql.Rows) (d models.Datapack, ok bool, err error) {
	if !rows.Next() {
		if err = rows.Err(); err != nil {
			err = fmt.Errorf("rows iteration error: %w", err)
		}
		return d, false, err
	}
	if err = scanDatapackRow(rows, &d); err != nil {
		return d, false, fmt.Errorf("failed to scan datapack: %w", err)
	}
	return d, true, nil
}

// StreamAll calls fn for every datapack userID can see, newest first, without
// holding the list. Owned and shared rows are each read in that order and
// merged as they arrive. An error from fn stops the stream and is returned.
func (r *DatapackRepo) StreamAll(ctx context.Context, userID string, fn func(models.Datapack) error) error {
	sq, sargs := sharedListQuery(userID, DatapackListOpts{Sort: DatapackSortCreated}, 0)
	sharedRows, err := r.db.QueryContext(ctx, sq, sargs...)
	if err != nil {
		return fmt.Errorf("failed to list datapacks: %w", err)
	}
	defer sharedRows.Close()

	name, email, err := r.ownerOf(ctx, userID)
	if err != nil {
		return err
	}

	ownedRows, err := r.db.QueryContext(ctx,
		`SELECT `+ownedColumns+`
		 FROM meko_system.datapacks d
		 WHERE d.user_id = $1
		 ORDER BY d.created_at DESC, d.datapack_id DESC`,
		userID,
	)
	if err != nil {
		return fmt.Errorf("failed to list datapacks: %w", err)
	}
	defer ownedRows.Close()

	shared, haveShared, err := nextDatapack(sharedRows)
	if err != nil {
		return err
	}
	owned, haveOwned, err := nextDatapack(ownedRows)
	if err != nil {
		return err
	}
	for haveShared || haveOwned {
		if haveShared && (!haveOwned || datapackBefore(DatapackSortCreated, &shared, &owned)) {
			if err := fn(shared); err != nil {
				return err
			}
			shared, haveShared, err = nextDatapack(sharedRows)
			if err != nil {
				return err
			}
			continue
		}
		owned.OwnerName, owned.OwnerEmail = name, email
		if err := fn(owned); err != nil {
			return err
		}
		owned, haveOwned, err = nextDatapack(ownedRows)
		if err != nil {
			return err
		}
	}
	return nil
}

// VisibleByIDs returns the datapacks among ids that userID can see, by the
// list's definition of visible. Unknown and invisible ids are left out.
func (r *DatapackRepo) VisibleByIDs(ctx context.Context, userID string, ids []string) ([]models.Datapack, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	owned, err := r.queryDatapacks(ctx,
		`SELECT `+ownedColumns+`
		 FROM meko_system.datapacks d
		 WHERE d.user_id = $1 AND d.datapack_id = ANY($2::uuid[])`,
		[]any{userID, pq.Array(ids)},
	)
	if err != nil {
		return nil, err
	}
	if len(owned) > 0 {
		name, email, oErr := r.ownerOf(ctx, userID)
		if oErr != nil {
			return nil, oErr
		}
		for i := range owned {
			owned[i].OwnerName, owned[i].OwnerEmail = name, email
		}
	}
	shared, err := r.queryDatapacks(ctx,
		`WITH `+sharedInvitesCTE+`
		 SELECT `+datapackColumns+`, s.grant_role
		 FROM shared s
		 JOIN meko_system.datapacks d ON d.datapack_id = s.datapack_id
		 JOIN meko_system.users u ON u.user_id = d.user_id
		 WHERE d.user_id <> $1 AND d.datapack_id = ANY($2::uuid[])`,
		[]any{userID, pq.Array(ids)},
	)
	if err != nil {
		return nil, err
	}
	return append(owned, shared...), nil
}

// SharedIDs returns the ids of the datapacks shared with userID.
func (r *DatapackRepo) SharedIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`WITH `+sharedInvitesCTE+`
		 SELECT d.datapack_id
		 FROM shared s
		 JOIN meko_system.datapacks d ON d.datapack_id = s.datapack_id
		 WHERE d.user_id <> $1`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list shared datapack ids: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("failed to scan shared datapack id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iteration error: %w", err)
	}
	return out, nil
}
