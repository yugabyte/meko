package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

const managedEnvID = "amp-managed-use1"

// deregisterMethods are the two routes that deregister: DELETE, and PATCH with
// {"action":"deregister"}. They are one operation, so both carry the guard.
var deregisterMethods = []string{http.MethodDelete, http.MethodPatch}

func deregisterRequest(
	t *testing.T, h *EnvironmentHandler, method, query, accountID string,
) *httptest.ResponseRecorder {
	t.Helper()
	handler, body := h.Deregister, ""
	if method == http.MethodPatch {
		handler, body = h.Action, `{"action":"deregister"}`
	}
	return fleetRequest(t, handler, method, "/api/v1/environments/"+managedEnvID+query,
		accountID, body, managedEnvID)
}

func expectUnscopedManagedGet(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`FROM meko_system\.environments e WHERE e\.id = \$1 LIMIT 2`).
		WithArgs(managedEnvID).
		WillReturnRows(managedRow(""))
}

func wantRefusal(t *testing.T, rec *httptest.ResponseRecorder, mock sqlmock.Sqlmock, status int, code string) {
	t.Helper()
	if rec.Code != status || !strings.Contains(rec.Body.String(), code) {
		t.Fatalf("status = %d, want %d %s: %s", rec.Code, status, code, rec.Body.String())
	}
	// Only the lookup was registered, so a write would have failed the request.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestDeregister_ManagedWithoutForceIsConflict(t *testing.T) {
	for _, method := range deregisterMethods {
		t.Run(method, func(t *testing.T) {
			h, mock, done := newScopeHandler(t)
			defer done()
			expectUnscopedManagedGet(mock)
			wantRefusal(t, deregisterRequest(t, h, method, "", ""), mock,
				http.StatusConflict, "MANAGED_ENVIRONMENT")
		})
	}
}

// A malformed force is not a force.
func TestDeregister_ManagedWithMalformedForceIsConflict(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()
	expectUnscopedManagedGet(mock)
	wantRefusal(t, deregisterRequest(t, h, http.MethodDelete, "?force=yes", ""), mock,
		http.StatusConflict, "MANAGED_ENVIRONMENT")
}

func TestDeregister_ManagedWithForceDeregisters(t *testing.T) {
	for _, method := range deregisterMethods {
		t.Run(method, func(t *testing.T) {
			h, mock, done := newScopeHandler(t)
			defer done()
			expectUnscopedManagedGet(mock)
			mock.ExpectExec(`UPDATE meko_system\.environments SET`).
				WillReturnResult(sqlmock.NewResult(0, 1))

			rec := deregisterRequest(t, h, method, "?force=true", "")
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), keyA) {
				t.Fatalf("status = %d, want 200 carrying api_key_id %s: %s", rec.Code, keyA, rec.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet expectations: %v", err)
			}
		})
	}
}

// An operator can stamp a MANAGED environment with an account, which makes it visible to
// that account's scoped calls; force still does not let them deregister it.
func TestDeregister_ManagedRefusesAScopedCaller(t *testing.T) {
	for _, method := range deregisterMethods {
		t.Run(method, func(t *testing.T) {
			h, mock, done := newScopeHandler(t)
			defer done()
			mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
				WithArgs(acctA, managedEnvID).
				WillReturnRows(managedRow(acctA))
			wantRefusal(t, deregisterRequest(t, h, method, "?force=true", acctA), mock,
				http.StatusForbidden, "MANAGED_REQUIRES_OPERATOR")
		})
	}
}
