package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

const listTestUser = "4f17936c-5f80-4142-9ff8-e7f3a4e500de"

var listCols = []string{
	"datapack_id", "user_id", "datapack_name", "created_by_agent_id", "universe_id",
	"conn", "mcp_user", "created_at", "updated_at", "description",
	"owner_name", "owner_email", "grant",
}

func newListHandler(t *testing.T) (*DatapackHandler, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	repo := repository.NewDatapackRepo(db, "", repository.DeployEnvProd)
	svc := service.NewDatapackService(repo, nil, nil, nil, nil, nil)
	return NewDatapackHandler(svc, nil, nil, nil), mock
}

// listServer serves the handler behind the same Recover middleware production
// uses, with the account set, so a streamed response is read as a client would.
func listServer(h *DatapackHandler) *httptest.Server {
	e := echo.New()
	e.Use(middleware.Recover())
	e.GET("/datapacks", h.List, func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(auth.ContextKeyAccount, &models.Account{ID: listTestUser})
			return next(c)
		}
	})
	e.GET("/datapacks/count", h.Count, func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(auth.ContextKeyAccount, &models.Account{ID: listTestUser})
			return next(c)
		}
	})
	return httptest.NewServer(e)
}

func get(t *testing.T, srv *httptest.Server, path string) (int, []byte, error) {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

// mustGet is get for a response that must be read in full.
func mustGet(t *testing.T, srv *httptest.Server, path string) (int, []byte) {
	t.Helper()
	status, body, err := get(t, srv, path)
	if err != nil {
		t.Fatalf("GET %s: read body: %v", path, err)
	}
	return status, body
}

func ownedRow(rows *sqlmock.Rows, id, name string, at time.Time) {
	rows.AddRow(id, listTestUser, name, "agent", "univ", "postgres://secret", "dbuser",
		at, at, "desc", "", "", "owner")
}

// expectStream stubs StreamAll: shared rows first, the owner lookup, then the
// owned stream.
func expectStream(mock sqlmock.Sqlmock, shared, owned *sqlmock.Rows) {
	mock.ExpectQuery("WITH shared AS").WithArgs(listTestUser).WillReturnRows(shared)
	mock.ExpectQuery("FROM meko_system.users WHERE user_id").WithArgs(listTestUser).
		WillReturnRows(sqlmock.NewRows([]string{"n", "e"}).AddRow("Amiram M", "a@example.com"))
	mock.ExpectQuery(`ORDER BY d.created_at DESC, d.datapack_id DESC`).WithArgs(listTestUser).
		WillReturnRows(owned)
}

// The streamed legacy list must be byte for byte what ok(c, 200, datapacks)
// wrote before streaming, for 0, 1 and several rows, with the connection
// fields redacted.
func TestDatapackList_StreamMatchesTheOldResponse(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, n := range []int{0, 1, 5} {
		h, mock := newListHandler(t)
		owned := sqlmock.NewRows(listCols)
		shared := sqlmock.NewRows(listCols)
		var want []models.Datapack
		for i := 0; i < n; i++ {
			id := strings.Repeat(string(rune('a'+i)), 8) + "-0000-0000-0000-000000000000"
			at := t0.Add(-time.Duration(i) * time.Hour)
			ownedRow(owned, id, "dp-"+id[:1], at)
			want = append(want, models.Datapack{
				ID: id, UserID: listTestUser, Name: "dp-" + id[:1], CreatedByAgentID: "agent",
				CreatedAt: at, UpdatedAt: at, Description: "desc",
				OwnerName: "Amiram M", OwnerEmail: "a@example.com", Grant: "owner",
			})
		}
		if n > 1 {
			// A shared row between the first and second owned ones.
			at := t0.Add(-30 * time.Minute)
			shared.AddRow("ffffffff-0000-0000-0000-000000000000", "other", "shared", "", "u", "c", "m",
				at, at, "", "Other", "o@example.com", "viewer")
			sh := models.Datapack{
				ID: "ffffffff-0000-0000-0000-000000000000", UserID: "other", Name: "shared",
				CreatedAt: at, UpdatedAt: at, OwnerName: "Other", OwnerEmail: "o@example.com", Grant: "viewer",
			}
			want = append([]models.Datapack{want[0], sh}, want[1:]...)
		}
		expectStream(mock, shared, owned)

		srv := listServer(h)
		status, body, err := get(t, srv, "/datapacks")
		srv.Close()
		if err != nil || status != http.StatusOK {
			t.Fatalf("n=%d: status %d, err %v", n, status, err)
		}

		// What the old handler wrote for the same rows.
		if want == nil {
			want = []models.Datapack{}
		}
		rec := httptest.NewRecorder()
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		if err := ok(c, http.StatusOK, want); err != nil {
			t.Fatal(err)
		}
		if string(body) != rec.Body.String() {
			t.Fatalf("n=%d: streamed body differs\n got: %s\nwant: %s", n, body, rec.Body.String())
		}
		if strings.Contains(string(body), "secret") || strings.Contains(string(body), "dbuser") {
			t.Fatalf("n=%d: connection details leaked: %s", n, body)
		}
	}
}

// A failure before anything is written is the normal error envelope.
func TestDatapackList_StreamErrorBeforeFirstRowIs500(t *testing.T) {
	h, mock := newListHandler(t)
	mock.ExpectQuery("WITH shared AS").WillReturnError(errors.New("db down"))
	srv := listServer(h)
	defer srv.Close()

	status, body, err := get(t, srv, "/datapacks")
	if err != nil || status != http.StatusInternalServerError {
		t.Fatalf("status %d, err %v", status, err)
	}
	var env APIResponse
	if json.Unmarshal(body, &env) != nil || env.Error == nil || env.Error.Code != "INTERNAL_ERROR" {
		t.Fatalf("body = %s", body)
	}
}

// A failure after rows went out must not end in a well-formed short list: the
// client would take it as complete, and ycp deletes what the list leaves out.
func TestDatapackList_StreamErrorMidwayAbortsTheResponse(t *testing.T) {
	h, mock := newListHandler(t)
	t0 := time.Now()
	owned := sqlmock.NewRows(listCols)
	for i := 0; i < 600; i++ { // past a flush, so the client has bytes in hand
		ownedRow(owned, "aaaaaaaa-0000-0000-0000-000000000000", "dp", t0)
	}
	owned.RowError(599, errors.New("tablet gone"))
	expectStream(mock, sqlmock.NewRows(listCols), owned)
	srv := listServer(h)
	defer srv.Close()

	status, body, err := get(t, srv, "/datapacks")
	if status != http.StatusOK {
		t.Fatalf("status %d; the stream had started", status)
	}
	if err == nil {
		var v any
		if json.Unmarshal(body, &v) == nil {
			t.Fatalf("a failed stream parsed as complete JSON (%d bytes)", len(body))
		}
	}
}

// limit= and limit=<spaces> are how some clients send an unset parameter.
// That request is the legacy list, not a 400.
func TestDatapackList_EmptyLimitIsLegacy(t *testing.T) {
	for _, q := range []string{"limit=", "limit=%20"} {
		h, mock := newListHandler(t)
		expectStream(mock, sqlmock.NewRows(listCols), sqlmock.NewRows(listCols))
		srv := listServer(h)
		status, body, err := get(t, srv, "/datapacks?"+q)
		srv.Close()
		if err != nil || status != http.StatusOK {
			t.Fatalf("%s: status %d, err %v, body %s", q, status, err, body)
		}
		if !strings.Contains(string(body), `"data":[`) {
			t.Fatalf("%s: body = %s", q, body)
		}
	}
}

func TestDatapackList_PagedParamValidation(t *testing.T) {
	for _, c := range []struct {
		query, code string
	}{
		{"limit=0", "INVALID_LIMIT"},
		{"limit=-1", "INVALID_LIMIT"},
		{"limit=ten", "INVALID_LIMIT"},
		{"limit=5&sort=recent", "INVALID_SORT"},
		{"limit=5&grant=admin", "INVALID_GRANT"},
		{"limit=5&fields=all", "INVALID_FIELDS"},
		{"limit=5&include_total=maybe", "INVALID_INCLUDE_TOTAL"},
		{"limit=5&q=" + strings.Repeat("x", 201), "INVALID_QUERY"},
		{"limit=5&cursor=not-base64!", "INVALID_CURSOR"},
	} {
		h, _ := newListHandler(t)
		srv := listServer(h)
		status, body := mustGet(t, srv, "/datapacks?"+c.query)
		srv.Close()
		var env APIResponse
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("%s: body %s: %v", c.query, body, err)
		}
		if status != http.StatusBadRequest || env.Error == nil || env.Error.Code != c.code {
			t.Errorf("%s: status %d body %s, want 400 %s", c.query, status, body, c.code)
		}
	}
}

// A cursor from one sort or filter set is refused on another rather than
// silently restarting.
func TestDatapackList_CursorBoundToSortAndFilters(t *testing.T) {
	key := repository.DatapackCursorKey{ID: "aaaaaaaa-0000-0000-0000-000000000000", CreatedAt: time.Now()}
	cur := repository.EncodeDatapackCursor("created", repository.DatapackFilterHash("", "", ""), key)
	for _, q := range []string{"sort=name", "q=x", "grant=owner", "name=y"} {
		h, _ := newListHandler(t)
		srv := listServer(h)
		status, body := mustGet(t, srv, "/datapacks?limit=5&cursor="+cur+"&"+q)
		srv.Close()
		if status != http.StatusBadRequest || !strings.Contains(string(body), "INVALID_CURSOR") {
			t.Errorf("%s: status %d body %s", q, status, body)
		}
	}
}

func pagedOwned(mock sqlmock.Sqlmock, n int) {
	rows := sqlmock.NewRows(listCols)
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		id := strings.Repeat(string(rune('a'+i)), 8) + "-0000-0000-0000-000000000000"
		ownedRow(rows, id, "dp", t0.Add(-time.Duration(i)*time.Minute))
	}
	mock.ExpectQuery(`FROM meko_system.datapacks d\s+WHERE d.user_id = \$1`).WillReturnRows(rows)
	mock.ExpectQuery("FROM meko_system.users WHERE user_id").
		WillReturnRows(sqlmock.NewRows([]string{"n", "e"}).AddRow("Amiram M", "a@example.com"))
	mock.ExpectQuery("WITH shared AS").WillReturnRows(sqlmock.NewRows(listCols))
}

func TestDatapackList_PagedEnvelope(t *testing.T) {
	h, mock := newListHandler(t)
	pagedOwned(mock, 3) // limit 2 + 1 tells the page there is more
	srv := listServer(h)
	defer srv.Close()

	status, body, err := get(t, srv, "/datapacks?limit=2")
	if err != nil || status != http.StatusOK {
		t.Fatalf("status %d err %v body %s", status, err, body)
	}
	var env struct {
		Data struct {
			Total      *int64           `json:"total"`
			NextCursor string           `json:"next_cursor"`
			Items      []map[string]any `json:"items"`
			Limit      int              `json:"limit"`
			HasMore    bool             `json:"has_more"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	d := env.Data
	if len(d.Items) != 2 || !d.HasMore || d.NextCursor == "" || d.Limit != 2 || d.Total != nil {
		t.Fatalf("envelope = %s", body)
	}
	for _, k := range []string{"universe_id", "datapack_ysql_connection_string", "memory_count", "knowledge_count"} {
		if _, has := d.Items[0][k]; has {
			t.Errorf("summary item carries %q", k)
		}
	}
	for _, k := range []string{"datapack_id", "datapack_name", "grant", "created_at", "user_id", "datapack_owner_name"} {
		if _, has := d.Items[0][k]; !has {
			t.Errorf("summary item lacks %q", k)
		}
	}
	if !strings.Contains(string(body), `"has_more":true`) || strings.Contains(string(body), "secret") {
		t.Fatalf("body = %s", body)
	}
}

func TestDatapackList_PagedFullIsRedacted(t *testing.T) {
	h, mock := newListHandler(t)
	pagedOwned(mock, 1)
	srv := listServer(h)
	defer srv.Close()

	_, body := mustGet(t, srv, "/datapacks?limit=1000000&fields=full")
	if !strings.Contains(string(body), `"datapack_ysql_connection_string":""`) ||
		strings.Contains(string(body), "secret") || strings.Contains(string(body), "dbuser") {
		t.Fatalf("body = %s", body)
	}
	if !strings.Contains(string(body), `"limit":1000`) || !strings.Contains(string(body), `"next_cursor":""`) {
		t.Fatalf("limit not clamped or last page carries a cursor: %s", body)
	}
}

func TestDatapackCount(t *testing.T) {
	h, mock := newListHandler(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM meko_system.datapacks d WHERE d.user_id = \$1$`).
		WithArgs(listTestUser).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1000000))
	mock.ExpectQuery("WITH shared AS").
		WithArgs(listTestUser).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(3))
	srv := listServer(h)
	defer srv.Close()

	_, body := mustGet(t, srv, "/datapacks/count")
	if strings.TrimSpace(string(body)) != `{"data":{"owned":1000000,"shared":3,"total":1000003}}` {
		t.Fatalf("body = %s", body)
	}
}
