package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/labstack/echo/v4"

	"meko/api_server/internal/auth"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

func newTasksHandler(t *testing.T) (*EnvironmentTasksHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	svc := service.NewEnvironmentService(
		repository.NewEnvironmentRepo(db), repository.NewEnvironmentTaskRepo(db))
	return NewEnvironmentTasksHandler(svc), mock, func() { db.Close() }
}

// tasksRequest runs one tasks request, optionally with a :task_id path param, which
// fleetRequest does not model.
func tasksRequest(
	t *testing.T, h echo.HandlerFunc, target, accountID, envID, taskID string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, strings.NewReader(""))
	if accountID != "" {
		req.Header.Set(auth.AccountIDHeader, accountID)
	}
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	names, values := []string{"id"}, []string{envID}
	if taskID != "" {
		names, values = append(names, "task_id"), append(values, taskID)
	}
	c.SetParamNames(names...)
	c.SetParamValues(values...)
	if err := h(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	return rec
}

func tasksTarget(params url.Values) string {
	return "/api/v1/environments/acme-prod/tasks?" + params.Encode()
}

var taskRowCols = []string{
	"task_id", "kind", "status", "cluster_id", "target_ref", "error",
	"result", "ycp_generation", "task_created_at", "task_updated_at", "reported_at",
}

// decodeTaskList unwraps the {data,error} envelope the fleet routes answer in.
func decodeTaskList(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var env struct {
		Data  map[string]any `json:"data"`
		Error any            `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode body %s: %v", rec.Body.String(), err)
	}
	if env.Error != nil {
		t.Fatalf("response carries an error: %s", rec.Body.String())
	}
	return env.Data
}

// TestTasks_OtherAccountIsNotFound is the authorization test. Another account's
// environment is a 404 exactly as on the sibling fleet routes — AND the task table
// is never queried, so the 404 cannot be told from a real miss by timing or by
// whether anything came back.
func TestTasks_OtherAccountIsNotFound(t *testing.T) {
	h, mock, done := newTasksHandler(t)
	defer done()

	mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
		WithArgs(acctB, "acme-prod").
		WillReturnRows(sqlmock.NewRows(scopeRowCols))

	rec := tasksRequest(t, h.List, tasksTarget(nil), acctB, "acme-prod", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	// Any query beyond the environment lookup is an unmet-expectation failure here,
	// since only that one was registered.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the task history was read for an environment the caller cannot see: %v", err)
	}
}

// TestTasks_ReadsTheResolvedRowNotTheQueryString: the history is scoped by the uuid
// of the row the account check resolved, so a caller cannot retarget the read by
// naming another environment's uuid in the query.
func TestTasks_ReadsTheResolvedRowNotTheQueryString(t *testing.T) {
	h, mock, done := newTasksHandler(t)
	defer done()

	mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
		WithArgs(acctA, "acme-prod").
		WillReturnRows(scopeRow("acme-prod", acctA))
	// envUUID is what scopeRow stores. The attacker-supplied one below must not appear.
	mock.ExpectQuery(`SELECT count\(\*\) FROM meko_system\.environment_tasks`).
		WithArgs(envUUID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`FROM meko_system\.environment_tasks`).
		WithArgs(envUUID, tasksDefaultPageSize, 0).
		WillReturnRows(sqlmock.NewRows(taskRowCols))

	params := url.Values{
		"env_uuid":   {"99999999-9999-9999-9999-999999999999"},
		"account_id": {acctB},
	}
	rec := tasksRequest(t, h.List, tasksTarget(params), acctA, "acme-prod", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the read did not use the resolved row: %v", err)
	}
}

// TestTasks_ActiveShorthand covers the console's primary query: ?active=true is the
// non-terminal pair, and it must reach SQL as exactly those two statuses.
func TestTasks_ActiveShorthand(t *testing.T) {
	h, mock, done := newTasksHandler(t)
	defer done()

	mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
		WithArgs(acctA, "acme-prod").
		WillReturnRows(scopeRow("acme-prod", acctA))
	mock.ExpectQuery(`status = ANY\(\$2\)`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	now := time.Now().UTC()
	mock.ExpectQuery(`FROM meko_system\.environment_tasks`).
		WillReturnRows(sqlmock.NewRows(taskRowCols).
			AddRow("t1", "resize_endpoint", "running", nil, "endpoint/a", nil, "", nil, now, now, now))

	params := url.Values{"active": {"true"}}
	rec := tasksRequest(t, h.List, tasksTarget(params), acctA, "acme-prod", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	data := decodeTaskList(t, rec)
	items, _ := data["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v, want one running task", data["items"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestTasks_UnknownStatusIsDroppedNotRejected: the house convention is that a stale
// client asking for a status this control plane does not know gets results rather
// than a 400 it cannot act on. Dropping it means NO status predicate is bound, so
// the count query has one arg.
func TestTasks_UnknownStatusIsDroppedNotRejected(t *testing.T) {
	h, mock, done := newTasksHandler(t)
	defer done()

	mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
		WithArgs(acctA, "acme-prod").
		WillReturnRows(scopeRow("acme-prod", acctA))
	mock.ExpectQuery(`SELECT count\(\*\) FROM meko_system\.environment_tasks WHERE env_uuid = \$1$`).
		WithArgs(envUUID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`FROM meko_system\.environment_tasks`).
		WithArgs(envUUID, tasksDefaultPageSize, 0).
		WillReturnRows(sqlmock.NewRows(taskRowCols))

	params := url.Values{"status": {"cancelled"}}
	rec := tasksRequest(t, h.List, tasksTarget(params), acctA, "acme-prod", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (an unknown status is dropped): %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("an unknown status reached SQL as a predicate: %v", err)
	}
}

// TestTasks_PageSizeClamps: page_size above the ceiling is clamped rather than
// honoured, and a nonsense page falls back to the default instead of computing a
// negative offset.
func TestTasks_PageSizeClamps(t *testing.T) {
	for _, tc := range []struct {
		name             string
		params           url.Values
		wantLim, wantOff int
	}{
		{"above the ceiling", url.Values{"page_size": {"9999"}}, tasksMaxPageSize, 0},
		{"at the ceiling", url.Values{"page_size": {"200"}}, tasksMaxPageSize, 0},
		{"negative page", url.Values{"page": {"-3"}}, tasksDefaultPageSize, 0},
		{"zero page", url.Values{"page": {"0"}}, tasksDefaultPageSize, 0},
		{"unparseable", url.Values{"page_size": {"lots"}}, tasksDefaultPageSize, 0},
		{"second page", url.Values{"page": {"3"}, "page_size": {"10"}}, 10, 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, mock, done := newTasksHandler(t)
			defer done()

			mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
				WithArgs(acctA, "acme-prod").
				WillReturnRows(scopeRow("acme-prod", acctA))
			mock.ExpectQuery(`SELECT count\(\*\)`).WithArgs(envUUID).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
			mock.ExpectQuery(`FROM meko_system\.environment_tasks`).
				WithArgs(envUUID, tc.wantLim, tc.wantOff).
				WillReturnRows(sqlmock.NewRows(taskRowCols))

			rec := tasksRequest(t, h.List, tasksTarget(tc.params), acctA, "acme-prod", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("limit/offset did not clamp as expected: %v", err)
			}
		})
	}
}

// TestTasks_MekoEnvironmentIsAnEmptyPage: a meko environment runs no controller and
// has no task log. The console renders one tab for every kind, so "nothing has run"
// and "this kind does not run tasks" must read alike rather than one breaking the tab.
func TestTasks_MekoEnvironmentIsAnEmptyPage(t *testing.T) {
	h, mock, done := newTasksHandler(t)
	defer done()

	mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
		WithArgs(acctA, "acme-prod").
		WillReturnRows(scopeRow("acme-prod", acctA)) // scopeRow's kind is "meko"
	mock.ExpectQuery(`SELECT count\(\*\)`).WithArgs(envUUID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`FROM meko_system\.environment_tasks`).
		WithArgs(envUUID, tasksDefaultPageSize, 0).
		WillReturnRows(sqlmock.NewRows(taskRowCols))

	rec := tasksRequest(t, h.List, tasksTarget(nil), acctA, "acme-prod", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	data := decodeTaskList(t, rec)
	// [] and not null: a console iterating the field breaks on null.
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Errorf("empty page serialized as %s, want items:[]", rec.Body.String())
	}
	if data["total"] != float64(0) || data["has_more"] != false {
		t.Errorf("empty page = total:%v has_more:%v", data["total"], data["has_more"])
	}
}

// TestTasks_GetOne covers the detail route, including the 404 for a task id that
// belongs to no task of this environment.
func TestTasks_GetOne(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		h, mock, done := newTasksHandler(t)
		defer done()

		now := time.Now().UTC()
		mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
			WithArgs(acctA, "acme-prod").
			WillReturnRows(scopeRow("acme-prod", acctA))
		mock.ExpectQuery(`WHERE env_uuid = \$1 AND task_id = \$2`).
			WithArgs(envUUID, "task-9").
			WillReturnRows(sqlmock.NewRows(taskRowCols).
				AddRow("task-9", "resize_endpoint", "failed", nil, "endpoint/a",
					"deploy timed out", "", nil, now, now, now))

		rec := tasksRequest(t, h.Get,
			"/api/v1/environments/acme-prod/tasks/task-9", acctA, "acme-prod", "task-9")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "deploy timed out") {
			t.Errorf("the failure reason did not reach the body: %s", rec.Body.String())
		}
	})

	t.Run("unknown task", func(t *testing.T) {
		h, mock, done := newTasksHandler(t)
		defer done()

		mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
			WithArgs(acctA, "acme-prod").
			WillReturnRows(scopeRow("acme-prod", acctA))
		mock.ExpectQuery(`WHERE env_uuid = \$1 AND task_id = \$2`).
			WithArgs(envUUID, "nope").
			WillReturnRows(sqlmock.NewRows(taskRowCols))

		rec := tasksRequest(t, h.Get,
			"/api/v1/environments/acme-prod/tasks/nope", acctA, "acme-prod", "nope")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("other account", func(t *testing.T) {
		h, mock, done := newTasksHandler(t)
		defer done()

		mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
			WithArgs(acctB, "acme-prod").
			WillReturnRows(sqlmock.NewRows(scopeRowCols))

		rec := tasksRequest(t, h.Get,
			"/api/v1/environments/acme-prod/tasks/task-9", acctB, "acme-prod", "task-9")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("a task was read for an environment the caller cannot see: %v", err)
		}
	})
}

// TestTasks_ReportsTheHeartbeat: the console needs last_heartbeat_at alongside the
// rows, or it cannot tell a task that has not moved from an agent that has stopped
// reporting.
func TestTasks_ReportsTheHeartbeat(t *testing.T) {
	h, mock, done := newTasksHandler(t)
	defer done()

	beat := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	row := sqlmock.NewRows(scopeRowCols).AddRow(
		envUUID, "acme-prod", acctA, "cloud", "us-east-1", "READY", "", 0, "",
		0, 400, "", beat, "", "", "[]", true, keyA, time.Now(), "amp", "[]", nil, "BYOC",
	)
	mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
		WithArgs(acctA, "acme-prod").WillReturnRows(row)
	mock.ExpectQuery(`SELECT count\(\*\)`).WithArgs(envUUID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`FROM meko_system\.environment_tasks`).
		WithArgs(envUUID, tasksDefaultPageSize, 0).
		WillReturnRows(sqlmock.NewRows(taskRowCols))

	rec := tasksRequest(t, h.List, tasksTarget(nil), acctA, "acme-prod", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	data := decodeTaskList(t, rec)
	if data["last_heartbeat_at"] == nil {
		t.Fatalf("last_heartbeat_at is absent: %s", rec.Body.String())
	}
	got, err := time.Parse(time.RFC3339, data["last_heartbeat_at"].(string))
	if err != nil {
		t.Fatalf("parse last_heartbeat_at: %v", err)
	}
	if !got.Equal(beat) {
		t.Errorf("last_heartbeat_at = %s, want %s", got, beat)
	}
}
