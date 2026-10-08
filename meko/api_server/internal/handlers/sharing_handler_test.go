package handlers_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"meko/api_server/internal/handlers"
)

// TestSharing_InviteAndOutgoing exercises the full invite flow:
// create a datapack, invite someone, and verify outgoing-shares lists
// it as pending.
func TestSharing_InviteAndOutgoing(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "share-test-dp")

	body := map[string]string{
		"recipient_email": "invitee@example.com",
		"grant":           "viewer",
	}
	b, _ := json.Marshal(body)

	resp, err := http.Post(
		srv.URL+"/api/v1/datapacks/share-test-dp/share",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatalf("POST /share failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var invResp handlers.APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&invResp); err != nil {
		t.Fatal(err)
	}
	data, ok := invResp.Data.(map[string]any)
	if !ok {
		t.Fatal("expected data to be a map")
	}
	if data["status"] != "pending" {
		t.Errorf("expected status=pending, got %v", data["status"])
	}
	if data["grant"] != "viewer" {
		t.Errorf("expected grant=viewer, got %v", data["grant"])
	}
	inviteID, _ := data["invite_id"].(string)
	if inviteID == "" {
		t.Error("expected non-empty invite_id")
	}

	outResp, err := http.Get(
		srv.URL + "/api/v1/datapacks/share-test-dp/outgoing-shares",
	)
	if err != nil {
		t.Fatalf("GET /outgoing-shares failed: %v", err)
	}
	defer outResp.Body.Close()
	if outResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", outResp.StatusCode)
	}

	var outResult handlers.APIResponse
	if err := json.NewDecoder(outResp.Body).Decode(&outResult); err != nil {
		t.Fatal(err)
	}
	outData, ok := outResult.Data.(map[string]any)
	if !ok {
		t.Fatal("expected data to be a map")
	}
	pending, ok := outData["pending_invites"].([]any)
	if !ok || len(pending) == 0 {
		t.Error("expected at least one pending invite in outgoing-shares")
	}
}

// TestSharing_InviteValidation_BadGrant verifies that an invalid grant
// returns 400.
func TestSharing_InviteValidation_BadGrant(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "share-bad-grant")

	body := map[string]string{
		"recipient_email": "invitee@example.com",
		"grant":           "superadmin",
	}
	b, _ := json.Marshal(body)

	resp, err := http.Post(
		srv.URL+"/api/v1/datapacks/share-bad-grant/share",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// TestSharing_InviteValidation_MissingEmail verifies that a missing
// email returns 400.
func TestSharing_InviteValidation_MissingEmail(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "share-no-email")

	body := map[string]string{
		"recipient_email": "",
		"grant":           "viewer",
	}
	b, _ := json.Marshal(body)

	resp, err := http.Post(
		srv.URL+"/api/v1/datapacks/share-no-email/share",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// TestSharing_InviteConflict verifies that inviting the same email
// twice to the same datapack returns 409.
func TestSharing_InviteConflict(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "share-dup")

	body := map[string]string{
		"recipient_email": "dup@example.com",
		"grant":           "contributor",
	}
	b, _ := json.Marshal(body)

	first, err := http.Post(
		srv.URL+"/api/v1/datapacks/share-dup/share",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first invite: expected 200, got %d", first.StatusCode)
	}

	second, err := http.Post(
		srv.URL+"/api/v1/datapacks/share-dup/share",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict, got %d", second.StatusCode)
	}
}

// postBulkShare POSTs an email list + grant to a datapack's bulk-share
// endpoint and returns the raw response. Callers must pass the datapack's
// UUID (not its display name) because RequireDatapackAccess parses the
// path param through uuid.Parse before the handler runs; a name here
// would 400 in the middleware and never reach the bulk-invite guards
// the tests are trying to exercise. See createDatapackAndReadID for a
// helper that fetches the UUID from the create response.
func postBulkShare(t *testing.T, baseURL, datapackID string, emails []string, grant string) *http.Response {
	t.Helper()
	b, err := json.Marshal(map[string]any{"emails": emails, "grant": grant})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(
		baseURL+"/api/v1/datapacks/"+datapackID+"/bulk-share",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatalf("POST /bulk-share failed: %v", err)
	}
	return resp
}

// createDatapackAndReadID creates a datapack by name and returns its
// server-assigned UUID. The single-invite tests happen to use the name
// in the URL and never hit the middleware's UUID guard because they run
// only against a live DB in CI (they skip locally without one), but the
// bulk-share route uses RequireDatapackAccess whose uuid.Parse rejects
// non-UUIDs with a 400 before the handler runs — so this helper exists
// so the new BulkInvite tests can talk to the same route the real
// client does.
func createDatapackAndReadID(t *testing.T, baseURL, name string) string {
	t.Helper()
	body := map[string]string{"name": name}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(
		baseURL+"/api/v1/datapacks", "application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %s: expected 201, got %d", name, resp.StatusCode)
	}
	var envelope handlers.APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("create %s: decode: %v", name, err)
	}
	data, ok := envelope.Data.(map[string]any)
	if !ok {
		t.Fatalf("create %s: data not a map, got %T", name, envelope.Data)
	}
	id, ok := data["datapack_id"].(string)
	if !ok || id == "" {
		t.Fatalf("create %s: no datapack_id in response body: %+v", name, data)
	}
	return id
}

// decodeBulkResults decodes a 200 bulk-share body into a status-by-email map
// plus the summary counts.
func decodeBulkResults(t *testing.T, resp *http.Response) (map[string]string, map[string]int) {
	t.Helper()
	var envelope handlers.APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	data, isMap := envelope.Data.(map[string]any)
	if !isMap {
		t.Fatalf("expected data to be a map, got %T", envelope.Data)
	}
	rows, isSlice := data["results"].([]any)
	if !isSlice {
		t.Fatalf("expected results to be an array, got %T", data["results"])
	}

	statuses := make(map[string]string, len(rows))
	for _, row := range rows {
		r, rowIsMap := row.(map[string]any)
		if !rowIsMap {
			t.Fatalf("expected result row to be a map, got %T", row)
		}
		email, emailIsStr := r["email"].(string)
		status, statusIsStr := r["status"].(string)
		if !emailIsStr || !statusIsStr {
			t.Fatalf("expected string email + status in row %+v", r)
		}
		statuses[email] = status
	}

	summary := map[string]int{}
	rawSummary, isMap := data["summary"].(map[string]any)
	if !isMap {
		t.Fatalf("expected summary to be a map, got %T", data["summary"])
	}
	for k, v := range rawSummary {
		n, isNum := v.(float64)
		if !isNum {
			t.Fatalf("expected summary[%q] to be a number, got %T", k, v)
		}
		summary[k] = int(n)
	}
	return statuses, summary
}

// TestSharing_BulkInvite exercises the partial-success contract: one call with
// a mixed list returns 200 and classifies every row independently.
//
// Note the dev/test server runs with Cognito unset, so a brand-new email can't
// be provisioned — that row is asserted loosely (invited* or error), while the
// rows that never touch Cognito are asserted exactly.
func TestSharing_BulkInvite(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	dpID := createDatapackAndReadID(t, srv.URL, "bulk-share-dp")

	// Seed one pending invite so the same email comes back as invite_exists.
	single, err := json.Marshal(map[string]string{
		"recipient_email": "existing-invite@example.com",
		"grant":           "viewer",
	})
	if err != nil {
		t.Fatal(err)
	}
	seed, err := http.Post(
		srv.URL+"/api/v1/datapacks/"+dpID+"/share",
		"application/json", bytes.NewReader(single),
	)
	if err != nil {
		t.Fatal(err)
	}
	seedStatus := seed.StatusCode
	seed.Body.Close()

	resp := postBulkShare(t, srv.URL, dpID, []string{
		"fresh@example.com",
		"existing-invite@example.com",
		"EXISTING-INVITE@example.com", // duplicate of the row above
		"not-an-email",
		"dev@localhost", // the dev owner's own address
	}, "viewer")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (partial success), got %d", resp.StatusCode)
	}

	statuses, summary := decodeBulkResults(t, resp)

	// The duplicate collapses, so 5 inputs yield 4 results.
	if len(statuses) != 4 {
		t.Errorf("expected 4 de-duplicated results, got %d: %v", len(statuses), statuses)
	}
	if got := statuses["not-an-email"]; got != "invalid_email" {
		t.Errorf("not-an-email: status = %q, want invalid_email", got)
	}
	if got := statuses["dev@localhost"]; got != "is_owner" {
		t.Errorf("dev@localhost: status = %q, want is_owner", got)
	}
	// Only meaningful if the seed invite actually landed.
	if seedStatus == http.StatusOK {
		if got := statuses["existing-invite@example.com"]; got != "invite_exists" {
			t.Errorf("existing-invite@example.com: status = %q, want invite_exists", got)
		}
	}
	switch got := statuses["fresh@example.com"]; got {
	case "invited", "invited_new_user", "error":
		// Any of these is valid depending on whether Cognito is wired up.
	default:
		t.Errorf("fresh@example.com: unexpected status %q", got)
	}

	// Summary must be a faithful tally of results.
	total := 0
	for _, n := range summary {
		total += n
	}
	if total != len(statuses) {
		t.Errorf("summary totals %d but there are %d results: %v", total, len(statuses), summary)
	}
}

// TestSharing_BulkInvite_BadGrant verifies the shared grant is validated once
// up front and fails the whole request rather than every row.
func TestSharing_BulkInvite_BadGrant(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	dpID := createDatapackAndReadID(t, srv.URL, "bulk-bad-grant")

	resp := postBulkShare(t, srv.URL, dpID,
		[]string{"a@example.com"}, "superadmin")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// TestSharing_BulkInvite_EmptyEmails verifies an empty list is rejected.
func TestSharing_BulkInvite_EmptyEmails(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	dpID := createDatapackAndReadID(t, srv.URL, "bulk-empty")

	resp := postBulkShare(t, srv.URL, dpID, []string{}, "viewer")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// TestSharing_BulkInvite_TooManyEmails verifies the per-request batch cap.
func TestSharing_BulkInvite_TooManyEmails(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	dpID := createDatapackAndReadID(t, srv.URL, "bulk-too-many")

	// One over the batch cap. Kept as a literal because this is an
	// external test package (handlers_test) and can't reach the
	// unexported maxBulkInviteEmails constant in the handlers package.
	// Keep in sync with maxBulkInviteEmails in sharing_handler.go.
	emails := make([]string, 51)
	for i := range emails {
		emails[i] = fmt.Sprintf("user%d@example.com", i)
	}

	resp := postBulkShare(t, srv.URL, dpID, emails, "viewer")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized batch, got %d", resp.StatusCode)
	}
}

// TestSharing_InviteAction_BadAction verifies that an invalid action
// returns 400.
func TestSharing_InviteAction_BadAction(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	body := map[string]string{
		"invite_id":         "d290f1ee-6c54-4b01-90e6-d701748f0851",
		"recipient_user_id": "b2c3d4e5-f6a7-8901-bcde-f12345678901",
		"user_action":       "maybe",
	}
	b, _ := json.Marshal(body)

	resp, err := http.Post(
		srv.URL+"/api/v1/datapacks/invite-action",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// TestSharing_InviteAction_NotFound verifies that acting on a
// non-existent invite returns 404.
func TestSharing_InviteAction_NotFound(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	body := map[string]string{
		"invite_id":         "00000000-0000-0000-0000-000000000000",
		"recipient_user_id": "b2c3d4e5-f6a7-8901-bcde-f12345678901",
		"user_action":       "accept",
	}
	b, _ := json.Marshal(body)

	resp, err := http.Post(
		srv.URL+"/api/v1/datapacks/invite-action",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// TestSharing_RevokeInvite verifies the revoke flow: create an invite,
// then revoke it.
func TestSharing_RevokeInvite(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "share-revoke")

	body := map[string]string{
		"recipient_email": "revoke@example.com",
		"grant":           "maintainer",
	}
	b, _ := json.Marshal(body)

	invResp, err := http.Post(
		srv.URL+"/api/v1/datapacks/share-revoke/share",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer invResp.Body.Close()

	var invResult handlers.APIResponse
	json.NewDecoder(invResp.Body).Decode(&invResult)
	invData, _ := invResult.Data.(map[string]any)
	inviteID, _ := invData["invite_id"].(string)

	req, err := http.NewRequest(
		http.MethodDelete,
		fmt.Sprintf("%s/api/v1/datapacks/share-revoke/invite/%s", srv.URL, inviteID),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	revokeResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer revokeResp.Body.Close()
	if revokeResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", revokeResp.StatusCode)
	}

	var revokeResult handlers.APIResponse
	json.NewDecoder(revokeResp.Body).Decode(&revokeResult)
	revokeData, _ := revokeResult.Data.(map[string]any)
	if revokeData["status"] != "revoked" {
		t.Errorf("expected status=revoked, got %v", revokeData["status"])
	}
}

// TestSharing_RevokeInvite_NotFound verifies revoking a non-existent
// invite returns 404.
func TestSharing_RevokeInvite_NotFound(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "share-revoke-404")

	req, err := http.NewRequest(
		http.MethodDelete,
		srv.URL+"/api/v1/datapacks/share-revoke-404/invite/00000000-0000-0000-0000-000000000000",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// TestSharing_IncomingShares_Empty verifies the dev user sees an empty
// array when no invites exist.
func TestSharing_IncomingShares_Empty(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	resp, err := http.Get(srv.URL + "/api/v1/datapacks/incoming-shares")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result handlers.APIResponse
	json.NewDecoder(resp.Body).Decode(&result)
	items, ok := result.Data.([]any)
	if !ok {
		t.Fatal("expected data to be an array")
	}
	if len(items) != 0 {
		t.Errorf("expected empty array, got %d items", len(items))
	}
}

// TestSharing_MemberList_OwnerOnly verifies that a fresh datapack's member-list
// contains only the owner in the members array.
func TestSharing_MemberList_OwnerOnly(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "share-owner-only")

	resp, err := http.Get(
		srv.URL + "/api/v1/datapacks/share-owner-only/member-list",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result handlers.APIResponse
	json.NewDecoder(resp.Body).Decode(&result)
	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatal("expected data to be a map")
	}

	members, ok := data["members"].([]any)
	if !ok {
		t.Fatal("expected members to be an array")
	}
	if len(members) != 1 {
		t.Fatalf("expected exactly 1 member (owner), got %d", len(members))
	}

	owner, ok := members[0].(map[string]any)
	if !ok {
		t.Fatal("expected member to be a map")
	}
	if owner["is_owner"] != true {
		t.Error("expected is_owner=true for the first member")
	}
}

// TestSharing_UpdateGrant_BadGrant verifies 400 for an invalid grant
// on the update endpoint.
func TestSharing_UpdateGrant_BadGrant(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "share-update-bad")

	body := map[string]string{"grant": "superadmin"}
	b, _ := json.Marshal(body)

	req, err := http.NewRequest(
		http.MethodPatch,
		srv.URL+"/api/v1/datapacks/share-update-bad/share/00000000-0000-0000-0000-000000000000",
		bytes.NewReader(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// TestSharing_DatapackNotFound verifies sharing routes on a
// non-existent datapack return 404 (via RequireDatapackOwnership).
func TestSharing_DatapackNotFound(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	body := map[string]string{
		"recipient_email": "nobody@example.com",
		"grant":           "viewer",
	}
	b, _ := json.Marshal(body)

	resp, err := http.Post(
		srv.URL+"/api/v1/datapacks/nonexistent-dp/share",
		"application/json", bytes.NewReader(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}
