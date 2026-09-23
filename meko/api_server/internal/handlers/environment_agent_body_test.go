package handlers

import (
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/labstack/echo/v4"
)

// CLOUDGA-36984: a status POST whose body was dropped in transit answered 204 and
// blanked the environment's state, because Echo's binder treats an absent body as a
// report of all-empty fields. Every case here asserts the status code AND that no
// UPDATE reached the DB -- the status code alone was never the bug.

// agentRequest runs one agent request through the handler. It cannot reuse
// fleetRequest, which hard-codes a JSON content type, and it seeds the api key id
// the way RequireAPIKeyID would: middleware does not run on a direct handler call.
// "meko_api_key_id" is the value of the unexported auth.apiKeyIDContextKey.
func agentRequest(
	t *testing.T, h echo.HandlerFunc, method, target, contentType, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.Set("meko_api_key_id", keyA)
	if err := h(c); err != nil {
		t.Fatalf("handler: %v", err)
	}
	return rec
}

// expectResolveAgent stands in for the lookup every agent route makes, by api key
// id, before it looks at the body.
func expectResolveAgent(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`WHERE e\.uuid = \(SELECT env_uuid FROM meko_system\.environment_api_keys`).
		WithArgs(keyA).
		WillReturnRows(scopeRow("acme-prod", acctA))
}

// expectApplyStatus adds the second lookup, by uuid, that ApplyStatus makes once it
// has accepted a report -- registered only where the report is expected to land.
func expectApplyStatus(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`FROM meko_system\.environments e WHERE e\.uuid = \$1`).
		WithArgs(envUUID).
		WillReturnRows(scopeRow("acme-prod", acctA))
}

func errBody(t *testing.T, rec *httptest.ResponseRecorder) (code, message string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not the JSON error envelope (%v): %s", err, rec.Body.String())
	}
	return body.Error.Code, body.Error.Message
}

// The probe from the ticket, verbatim: the same garbage payload that was correctly
// rejected as application/json was accepted when relabelled text/plain.
func TestAgentStatus_RejectsNonJSONContentType(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()
	expectResolveAgent(mock)

	rec := agentRequest(t, h.AgentStatus, http.MethodPost, "/api/v1/agent/status", "text/plain", "hello")

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415: %s", rec.Code, rec.Body.String())
	}
	if code, _ := errBody(t, rec); code != "UNSUPPORTED_MEDIA_TYPE" {
		t.Errorf("error code = %q, want UNSUPPORTED_MEDIA_TYPE", code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a rejected report still wrote: %v", err)
	}
}

// The shape the gateway actually delivered: the body was dropped, so the server saw
// Content-Length 0 and bound a zero-valued report.
func TestAgentStatus_RejectsEmptyBody(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()
	expectResolveAgent(mock)

	rec := agentRequest(t, h.AgentStatus, http.MethodPost, "/api/v1/agent/status", "application/json", "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if code, _ := errBody(t, rec); code != "EMPTY_BODY" {
		t.Errorf("error code = %q, want EMPTY_BODY", code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("an empty report still wrote: %v", err)
	}
}

// A well-formed report with no state is still rejected -- that is what a
// partially-dropped body produces. An unknown state is NOT rejected: see
// TestApplyStatus_StoresAStateItDoesNotKnow for why.
func TestAgentStatus_RejectsAbsentState(t *testing.T) {
	for name, body := range map[string]string{
		"empty":      `{"environment_state":""}`,
		"whitespace": `{"environment_state":"  "}`,
		"absent":     `{"agent_version":"v1.4.2"}`,
	} {
		t.Run(name, func(t *testing.T) {
			h, mock, done := newScopeHandler(t)
			defer done()
			expectResolveAgent(mock)

			rec := agentRequest(t, h.AgentStatus, http.MethodPost,
				"/api/v1/agent/status", "application/json", body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if code, _ := errBody(t, rec); code != "VALIDATION_ERROR" {
				t.Errorf("error code = %q, want VALIDATION_ERROR", code)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("an invalid state still wrote: %v", err)
			}
		})
	}
}

// The guard against over-correcting: a real heartbeat must still land.
func TestAgentStatus_AcceptsAValidReport(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()
	expectResolveAgent(mock)
	expectApplyStatus(mock)
	mock.ExpectExec(`UPDATE meko_system\.environments SET`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	rec := agentRequest(t, h.AgentStatus, http.MethodPost, "/api/v1/agent/status", "application/json",
		`{"environment_state":"READY","agent_version":"v1.4.2","components":[]}`)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a valid report did not write: %v", err)
	}
}

// Case is normalized rather than rejected: an agent shouting READY is unambiguous,
// and the column is what the console filters on.
func TestAgentStatus_NormalizesStateCase(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()
	expectResolveAgent(mock)
	expectApplyStatus(mock)
	// The state argument is pinned; the rest of the full-row overwrite is not what
	// this test is about.
	args := []driver.Value{envUUID, "cloud", "us-east-1", "READY"}
	for len(args) < 18 {
		args = append(args, sqlmock.AnyArg())
	}
	mock.ExpectExec(`UPDATE meko_system\.environments SET`).
		WithArgs(args...).
		WillReturnResult(sqlmock.NewResult(0, 1))

	rec := agentRequest(t, h.AgentStatus, http.MethodPost, "/api/v1/agent/status", "application/json",
		`{"environment_state":"ready"}`)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("state did not reach the column as READY: %v", err)
	}
}

// Enroll is the other write on this surface: a body-less call used to flip the
// environment live and into ATTESTING.
func TestAgentEnroll_RejectsEmptyBody(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	rec := agentRequest(t, h.AgentEnroll, http.MethodPost, "/api/v1/agent/enroll", "application/json", "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if code, _ := errBody(t, rec); code != "EMPTY_BODY" {
		t.Errorf("error code = %q, want EMPTY_BODY", code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a body-less enroll still wrote: %v", err)
	}
}

// The two agent-facing writes must agree about an unknown environment: AgentStatus
// answers 404 via StatusResult.Found, and this one discarded SetState's found bool
// and answered 204 -- a report that moved nothing, reported as success.
func TestAgentBootstrapProgress_UnknownEnvironmentIs404(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()
	expectResolveAgent(mock)
	// The row vanished between resolving the key and applying the phase.
	mock.ExpectQuery(`FROM meko_system\.environments e WHERE e\.uuid = \$1`).
		WithArgs(envUUID).
		WillReturnRows(sqlmock.NewRows(scopeRowCols))

	rec := agentRequest(t, h.AgentBootstrapProgress, http.MethodPost,
		"/api/v1/agent/bootstrap-progress", "application/json", `{"phase":"PROVISIONING"}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if code, _ := errBody(t, rec); code != "NOT_FOUND" {
		t.Errorf("error code = %q, want NOT_FOUND", code)
	}
}

// bootstrap-progress used to answer 204 to a report that moved nothing. An unknown
// phase still lands, for the same skew reason a status report's unknown state does.
func TestAgentBootstrapProgress_RejectsAbsentPhase(t *testing.T) {
	for name, body := range map[string]string{
		"absent":     `{"step":"x"}`,
		"empty":      `{"phase":""}`,
		"whitespace": `{"phase":"  "}`,
	} {
		t.Run(name, func(t *testing.T) {
			h, mock, done := newScopeHandler(t)
			defer done()
			expectResolveAgent(mock)

			rec := agentRequest(t, h.AgentBootstrapProgress, http.MethodPost,
				"/api/v1/agent/bootstrap-progress", "application/json", body)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if code, _ := errBody(t, rec); code != "VALIDATION_ERROR" {
				t.Errorf("error code = %q, want VALIDATION_ERROR", code)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("a bad phase still wrote: %v", err)
			}
		})
	}
}
