package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// A body that does not say what to run must change nothing.
//
// Every case here used to answer 202 and delete the environment's whole resource
// tree, which the agent then garbage-collected: an absent key, a misspelled one and
// a body-less request all decoded to an empty slice, indistinguishable from a
// deliberate "run nothing" (CLOUDGA-36999).
//
// The 400 check is the assertion doing the work — no expectations are registered on
// the mock, so a handler that reaches the database gets an error back and answers
// 500. ExpectationsWereMet cannot fail with nothing registered; it is here to catch
// a future edit that adds an expectation and stops exercising the refusal.
func TestSetDesired_RefusesBodiesThatDeclareNothing(t *testing.T) {
	cases := []struct {
		name string
		body string
		code string
	}{
		{"no body at all", "", "EMPTY_BODY"},
		{"no resources key", `{}`, "VALIDATION_ERROR"},
		{"the ticket's repro", `{"zzz":1}`, "INVALID_JSON"},
		{"explicit null", `{"resources":null}`, "VALIDATION_ERROR"},
		{"misspelled key", `{"resourses":[{"kind":"datapack","name":"alpha"}]}`, "INVALID_JSON"},
		// Strictness is not only top-level: the decoder walks the whole tree, so a
		// typo beside `kind` is refused like one beside `resources`. Pinned because
		// it holds only as long as one decoder handles the whole body — a later
		// refactor that unmarshals resources separately would drop it silently while
		// every other case here still passed.
		{"misspelled key inside a resource", `{"resources":[{"kind":"datapack","name":"a","kinnd":"x"}]}`, "INVALID_JSON"},
		{"empty set, unconfirmed", `{"resources":[]}`, "EMPTY_DESIRED_STATE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, mock, done := newScopeHandler(t)
			defer done()

			rec := fleetRequest(t, h.SetDesired, http.MethodPost,
				"/api/v1/environments/acme-prod/desired", acctA, tc.body, "acme-prod")

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			var resp APIResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.Error == nil || resp.Error.Code != tc.code {
				t.Errorf("error = %+v, want code %q", resp.Error, tc.code)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("a refused body still reached the database: %v", err)
			}
		})
	}
}

// Withdrawing everything stays reachable — it is how an operator takes a stack down,
// and how an environment is handed over to the chart-installed model. It just has to
// be asked for.
func TestSetDesired_EmptySetWithConfirmationWithdrawsEverything(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
		WithArgs(acctA, "acme-prod").
		WillReturnRows(scopeRow("acme-prod", acctA))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT kind, name FROM meko_system\.environment_desired_resources`).
		WithArgs(envUUID).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "name"}).AddRow("datapack", "alpha"))
	mock.ExpectExec(`DELETE FROM meko_system\.environment_desired_resources`).
		WithArgs(envUUID, "datapack", "alpha").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	rec := fleetRequest(t, h.SetDesired, http.MethodPost,
		"/api/v1/environments/acme-prod/desired", acctA,
		`{"resources":[],"allow_empty":true}`, "acme-prod")

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a confirmed empty set did not withdraw the existing resources: %v", err)
	}
}

// The ordinary declare path, with `generation` on the resource — the one field the
// read path emits that an operator editing a previous declaration is likely to send
// back. It is a real field on models.DesiredResource, so rejecting unknown fields
// must not reject it.
func TestSetDesired_AcceptsAReadModifyWriteRoundTrip(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
		WithArgs(acctA, "acme-prod").
		WillReturnRows(scopeRow("acme-prod", acctA))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT kind, name FROM meko_system\.environment_desired_resources`).
		WithArgs(envUUID).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "name"}))
	mock.ExpectExec(`INSERT INTO meko_system\.environment_desired_resources`).
		WithArgs(envUUID, "datapack", "alpha", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// `spec` is the boundary: json.RawMessage, so the decoder does not walk into it
	// and a key the control plane has never heard of rides through to the agent. That
	// is the contract every spec field depends on — anything else would make adding
	// one a control-plane change.
	rec := fleetRequest(t, h.SetDesired, http.MethodPost,
		"/api/v1/environments/acme-prod/desired", acctA,
		`{"resources":[{"kind":"datapack","name":"alpha",`+
			`"spec":{"description":"x","a_field_this_build_has_never_heard_of":1},"generation":3}]}`,
		"acme-prod")

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the declared resource was not written: %v", err)
	}
}
