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

// TestSharing_InviteAction_BadAction verifies that an invalid action
// returns 400.
func TestSharing_InviteAction_BadAction(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	body := map[string]string{
		"invite_id":        "d290f1ee-6c54-4b01-90e6-d701748f0851",
		"recipient_user_id": "b2c3d4e5-f6a7-8901-bcde-f12345678901",
		"user_action":      "maybe",
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
		"invite_id":        "00000000-0000-0000-0000-000000000000",
		"recipient_user_id": "b2c3d4e5-f6a7-8901-bcde-f12345678901",
		"user_action":      "accept",
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

// TestSharing_OutgoingShares_OwnerOnly verifies that a fresh datapack's
// outgoing-shares contains only the owner in the members array.
func TestSharing_OutgoingShares_OwnerOnly(t *testing.T) {
	srv, db := setupTestServer(t)
	defer srv.Close()
	defer db.Close()

	createDatapack(t, srv.URL, "share-owner-only")

	resp, err := http.Get(
		srv.URL + "/api/v1/datapacks/share-owner-only/outgoing-shares",
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
