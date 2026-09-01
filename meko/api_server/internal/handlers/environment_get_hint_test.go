package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// hintRow is a stored environment of the given kind, carrying the agent base URL
// its registration was given (empty for one registered without YBM in front).
func hintRow(id, kind, agentBaseURL string) *sqlmock.Rows {
	var stored any
	if agentBaseURL != "" {
		stored = agentBaseURL
	}
	return sqlmock.NewRows(scopeRowCols).AddRow(
		envUUID, id, acctA, "cloud", "us-east-1", "PENDING", "", 0, "",
		0, 400, "", nil, "", "", "[]", false, keyA, time.Now(), kind, "[]", stored,
	)
}

// getEnv runs a scoped get through the handler and returns the decoded environment.
func getEnv(t *testing.T, id string, rows *sqlmock.Rows) map[string]any {
	t.Helper()
	h, mock, done := newScopeHandler(t)
	defer done()

	mock.ExpectQuery(`WHERE e\.account_id = \$1 AND e\.id = \$2`).
		WithArgs(acctA, id).
		WillReturnRows(rows)

	rec := fleetRequest(t, h.Get, http.MethodGet, "/api/v1/environments/"+id, acctA, "", id)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Data
}

// Register's copy of the hint is gone the moment the operator navigates away, so
// the environment has to be able to hand it back — kind-aware, as at registration.
func TestGet_CarriesTheInstallHint(t *testing.T) {
	hint, _ := getEnv(t, "acme-prod", hintRow("acme-prod", "meko", ""))["install_hint"].(string)

	for _, want := range []string{
		"helm upgrade --install meko-agent charts/meko",
		"--set agent.ycpUrl=http://example.com/api/v1",
		"--set agent.envName=acme-prod",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("install_hint is missing %q:\n%s", want, hint)
		}
	}

	amp, _ := getEnv(t, "amp-acme", hintRow("amp-acme", "amp", ""))["install_hint"].(string)
	if !strings.Contains(amp, "helm upgrade --install yb-amp-controller") {
		t.Errorf("an amp environment must get the amp hint:\n%s", amp)
	}
}

// The pass-through base is the reason agent_base_url is stored rather than taken
// from the request: rendered from this call's address instead, the hint would point
// a YBM-registered agent at a URL it cannot reach.
func TestGet_HintDialsTheStoredAgentBaseURL(t *testing.T) {
	row := hintRow("acme-prod", "meko", "https://cloud.yugabyte.com/api/public/v1")
	hint, _ := getEnv(t, "acme-prod", row)["install_hint"].(string)

	want := "--set agent.ycpUrl=https://cloud.yugabyte.com/api/public/v1/accounts/" + acctA + "/ycp"
	if !strings.Contains(hint, want) {
		t.Errorf("install_hint is missing %q:\n%s", want, hint)
	}
}

// A fleet list must not carry it: the hint is a screenful of shell, and the console
// lists every environment an account holds.
func TestList_OmitsTheInstallHint(t *testing.T) {
	h, mock, done := newScopeHandler(t)
	defer done()

	mock.ExpectQuery(`WHERE e\.account_id = \$1 ORDER BY`).
		WithArgs(acctA).
		WillReturnRows(hintRow("acme-prod", "meko", ""))

	rec := fleetRequest(t, h.List, http.MethodGet, "/api/v1/environments", acctA, "", "")
	if strings.Contains(rec.Body.String(), "install_hint") {
		t.Errorf("the list carries an install hint: %s", rec.Body.String())
	}
}
