package main

import (
	"strings"
	"testing"

	"meko/ycp/internal/api"
)

// api_server wraps every response as {"data":…,"error":…}. Decoding that straight
// into the payload struct silently produced zero values, so the agent enrolled
// with an empty environment_id and every subsequent status report was rejected
// with 400. The flat form is still accepted: it is what the retired reference
// control plane returned, and tolerating both costs one type switch.
func TestDecodeBodyFlat(t *testing.T) {
	var er api.EnrollResponse
	in := `{"environment_id":"acme-prod","mcp_base_url":"acme.mcp"}`
	if err := decodeBody(strings.NewReader(in), &er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if er.EnvironmentID != "acme-prod" || er.MCPBaseURL != "acme.mcp" {
		t.Fatalf("flat body not decoded: %+v", er)
	}
}

func TestDecodeBodyEnveloped(t *testing.T) {
	var er api.EnrollResponse
	in := `{"data":{"environment_id":"acme-prod","environment_uuid":"6b1f0e1e-6f7a-4f4e-9d0a-3f2c1b5a7d90",` +
		`"mcp_base_url":"acme.mcp"},"error":null}`
	if err := decodeBody(strings.NewReader(in), &er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if er.EnvironmentID != "acme-prod" {
		t.Fatalf("envelope not unwrapped, environment_id = %q", er.EnvironmentID)
	}
	if er.EnvironmentUUID != "6b1f0e1e-6f7a-4f4e-9d0a-3f2c1b5a7d90" {
		t.Fatalf("envelope not unwrapped, environment_uuid = %q", er.EnvironmentUUID)
	}
	if er.MCPBaseURL != "acme.mcp" {
		t.Fatalf("envelope not unwrapped, mcp_base_url = %q", er.MCPBaseURL)
	}
}

// An envelope carrying an error must surface it rather than look like an empty
// success — that is what made the original failure so quiet.
func TestDecodeBodyEnvelopeError(t *testing.T) {
	var er api.EnrollResponse
	in := `{"data":null,"error":{"code":"UNAUTHORIZED","message":"no environment is bound to this api key"}}`
	err := decodeBody(strings.NewReader(in), &er)
	if err == nil {
		t.Fatal("expected an error from an envelope with error set")
	}
	if !strings.Contains(err.Error(), "no environment is bound") {
		t.Fatalf("error message not propagated: %v", err)
	}
}

// api_server omits "error" on success (it is `omitempty`), so the real success
// envelope is {"data":…} with NO "error" key. The agent must still unwrap it —
// requiring an "error" key was the bug that left the enroll response empty and
// got every status report rejected with 400.
func TestDecodeBodyEnvelopeWithoutErrorKey(t *testing.T) {
	var er api.EnrollResponse
	in := `{"data":{"environment_id":"acme-prod","mcp_base_url":"acme.mcp"}}`
	if err := decodeBody(strings.NewReader(in), &er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if er.EnvironmentID != "acme-prod" || er.MCPBaseURL != "acme.mcp" {
		t.Fatalf("success envelope (no error key) not unwrapped: %+v", er)
	}
}

func TestDecodeBodyEmpty(t *testing.T) {
	var er api.EnrollResponse
	if err := decodeBody(strings.NewReader(""), &er); err != nil {
		t.Fatalf("empty body should be a no-op, got %v", err)
	}
}
