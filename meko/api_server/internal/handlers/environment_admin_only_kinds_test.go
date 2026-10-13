package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/labstack/echo/v4"

	"meko/api_server/internal/repository"
)

// Public paths answer 400 for any request naming an admin-only kind. The cases that
// would otherwise be harmless — an unknown id, another account's environment, a copy
// of what is stored — are the ones pinning that the answer depends on the request
// alone. No expectation is registered, so a request that reached the database fails.
func TestSetDesired_RefusesAdminOnlyKinds(t *testing.T) {
	for name, body := range map[string]string{
		"controller":    `{"resources":[{"kind":"controller","name":"controller","spec":{"version":"2.1.0"}}]}`,
		"fleet-version": `{"resources":[{"kind":"fleet-version","name":"fleet","spec":{"version":"2.1.0"}}]}`,
		"identical to the stored row": `{"resources":[` +
			`{"kind":"controller","name":"controller","spec":{"version":"2.1.0"},"generation":2}]}`,
		"beside a storage pool": `{"resources":[{"kind":"storage-pool","name":"p"},` +
			`{"kind":"fleet-version","name":"fleet","spec":{"version":"2.1.0"}}]}`,
	} {
		for _, call := range []struct{ account, id string }{
			{acctA, "acme-prod"}, {acctB, "acme-prod"}, {"", "acme-prod"}, {acctA, "no-such-env"},
		} {
			t.Run(name+"/"+call.account+"/"+call.id, func(t *testing.T) {
				h, mock, done := newScopeHandler(t)
				defer done()
				rec := fleetRequest(t, h.SetDesired, http.MethodPost,
					"/api/v1/environments/"+call.id+"/desired", call.account, body, call.id)
				wantCode(t, rec, http.StatusBadRequest, "VALIDATION_ERROR")
				if err := mock.ExpectationsWereMet(); err != nil {
					t.Error(err)
				}
			})
		}
	}
}

// A scoped MANAGED registration would be a 403; naming an admin-only kind is a 400
// before that, and before the insert whose desired-state write swallows errors.
func TestRegister_RefusesAdminOnlyKinds(t *testing.T) {
	for _, account := range []string{"", acctA} {
		h, mock, done := newScopeHandler(t)
		rec := fleetRequest(t, h.Register, http.MethodPost, "/api/v1/environments", account,
			`{"name":"acme-prod","kind":"amp","type":"MANAGED","api_key_id":"`+keyA+`",`+
				`"desired":[{"kind":"controller","name":"controller","spec":{"version":"2.1.0"}}]}`, "")
		wantCode(t, rec, http.StatusBadRequest, "VALIDATION_ERROR")
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("account %q: a refused registration reached the database: %v", account, err)
		}
		done()
	}
}

// A confirmed empty set withdraws every resource and leaves the versions: a public
// read never shows them, so a read-modify-write could not have resent them.
func TestSetDesired_EmptySetLeavesAdminOnlyRows(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()
	mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
		WithArgs(acctA, "acme-prod").
		WillReturnRows(ampRow("acme-prod", acctA))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT kind, name FROM meko_system\.environment_desired_resources`).
		WithArgs(envUUID).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "name"}).
			AddRow("controller", "controller").
			AddRow("fleet-version", "fleet").
			AddRow("storage-pool", "p"))
	mock.ExpectExec(`DELETE FROM meko_system\.environment_desired_resources`).
		WithArgs(envUUID, "storage-pool", "p").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	rec := fleetRequest(t, h.SetDesired, http.MethodPost, "/api/v1/environments/acme-prod/desired", acctA,
		`{"resources":[],"allow_empty":true}`, "acme-prod")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("only the storage pool may be withdrawn: %v", err)
	}
}

// adminOnlyDesiredRows is a stored set holding both admin-only rows beside a pool.
func adminOnlyDesiredRows() *sqlmock.Rows {
	return desiredRows().
		AddRow("cluster", "p/c", "{}", int64(1)).
		AddRow("controller", "controller", `{"version":"2.1.0"}`, int64(5)).
		AddRow("fleet-version", "fleet", `{"version":"2.1.0"}`, int64(2)).
		AddRow("storage-pool", "p", "{}", int64(0))
}

func desiredKinds(t *testing.T, raw []byte) []string {
	t.Helper()
	var body struct {
		Data struct {
			Resources []struct {
				Kind string `json:"kind"`
			} `json:"resources"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	kinds := make([]string, 0, len(body.Data.Resources))
	for _, r := range body.Data.Resources {
		kinds = append(kinds, r.Kind)
	}
	return kinds
}

func TestGetDesired_OmitsAdminOnlyKinds(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()
	expectScopedEnv(mock, ampRow("acme-prod", acctA))
	mock.ExpectQuery(`FROM meko_system\.environment_desired_resources`).
		WithArgs(envUUID).WillReturnRows(adminOnlyDesiredRows())

	rec := fleetRequest(t, h.GetDesired, http.MethodGet, "/api/v1/environments/acme-prod/desired", acctA, "", "acme-prod")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := strings.Join(desiredKinds(t, rec.Body.Bytes()), ","); got != "cluster,storage-pool" {
		t.Errorf("kinds = %s, want cluster,storage-pool", got)
	}
}

// The agent is what converges the versions, so its read keeps them, and its revision
// moves when only a version does.
func TestAgentDesiredState_ServesAdminOnlyKinds(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()
	expectResolveAmpAgent(mock)
	mock.ExpectQuery(`FROM meko_system\.environment_desired_resources`).
		WithArgs(envUUID).WillReturnRows(adminOnlyDesiredRows())

	rec := agentRequest(t, h.AgentDesiredState, http.MethodGet, "/api/v1/agent/desired-state", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := strings.Join(desiredKinds(t, rec.Body.Bytes()), ",")
	if got != "cluster,controller,fleet-version,storage-pool" {
		t.Errorf("kinds = %s, want every stored row", got)
	}
	if data := extensionsBody(t, rec.Body.Bytes()); string(data["revision"]) != "5" {
		t.Errorf("revision = %s, want 5, the controller's generation", data["revision"])
	}
}

// The resources LIST is a tenant's view of a shared environment; the versions are the
// environment's own, on both the desired and the observed side, by kind whatever
// their name.
func TestListResourcesByName_OmitsAdminOnlyKinds(t *testing.T) {
	observed := `[{"kind":"storage-pool","id":"storage-pool/acct-1","name":"acct-1"},` +
		`{"kind":"controller","id":"controller/acct-1/ctl","name":"ctl"},` +
		`{"kind":"fleet-version","id":"fleet-version/acct-1","name":"acct-1"}]`
	for _, tc := range []struct{ query, want string }{
		{"name=acct-1&subtree=true", "storage-pool"},
		{"name=acct-1&subtree=true&kind=controller", ""},
	} {
		t.Run(tc.query, func(t *testing.T) {
			h, mock, done := newScopeHandler(t)
			defer done()
			mock.ExpectQuery(`FROM meko_system\.environment_aeon_account_mapping`).
				WithArgs("acct-1").
				WillReturnRows(sqlmock.NewRows([]string{"environment_id", "region"}).AddRow(envUUID, "us-east-1"))
			mock.ExpectQuery(`WHERE e\.uuid = \$1`).WithArgs(envUUID).WillReturnRows(ampResourceRow(observed))
			mock.ExpectQuery(`FROM meko_system\.environment_desired_resources`).
				WithArgs(envUUID).
				WillReturnRows(desiredRows().
					AddRow("controller", "acct-1/ctl", `{"version":"2.1.0"}`, int64(0)).
					AddRow("fleet-version", "acct-1", `{"version":"2.1.0"}`, int64(0)).
					AddRow("storage-pool", "acct-1", "{}", int64(0)))

			rec := fleetRequest(t, h.ListResourcesByName, http.MethodGet,
				"/api/v1/environments/resources?"+tc.query, "", "", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			env := decodeEnvResources(t, rec)[0]
			for side, rows := range map[string][]map[string]any{"desired": env.Desired, "observed": env.Observed} {
				kinds := make([]string, 0, len(rows))
				for _, r := range rows {
					kinds = append(kinds, fmt.Sprint(r["kind"]))
				}
				if got := strings.Join(kinds, ","); got != tc.want {
					t.Errorf("%s kinds = %q, want %q", side, got, tc.want)
				}
			}
		})
	}
}

// Every intent is refused, a delete included, before the transaction opens.
func TestSetResourcesByRegion_RefusesAdminOnlyKinds(t *testing.T) {
	for name, patch := range map[string]string{
		"flagless": `{"kind":"controller","name":"` + poolUUID + `/x","spec":{"version":"1"}}`,
		"create":   `{"kind":"fleet-version","name":"` + poolUUID + `","spec":{"version":"1"},"create":true}`,
		"update":   `{"kind":"controller","name":"` + poolUUID + `","spec":{"version":"1"},"update":true}`,
		"delete":   `{"kind":"fleet-version","name":"` + poolUUID + `","delete":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			h, mock, done := newScopeHandler(t)
			defer done()
			body := `{"storage_pool":"` + poolUUID + `","regions":[{"region":"us-east-1","resources":[` + patch + `]}]}`
			rec := fleetRequest(t, h.SetResourcesByRegion, http.MethodPatch, "/api/v1/environments/resources", "", body, "")
			wantCode(t, rec, http.StatusBadRequest, "VALIDATION_ERROR")
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("a refused patch reached the database: %v", err)
			}
		})
	}
}

// The service refuses first, so the repository's refusal arrives only if a caller
// skips it — and from inside the transaction it must still be the caller's 400.
func TestRegionPatchError_RepositoryAdminOnlyRefusalIs400(t *testing.T) {
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequest(http.MethodPatch, "/", nil), rec)
	if err := regionPatchError(c, fmt.Errorf("apply: %w: controller/p", repository.ErrAdminOnlyKind)); err != nil {
		t.Fatalf("regionPatchError: %v", err)
	}
	wantCode(t, rec, http.StatusBadRequest, "VALIDATION_ERROR")
}
