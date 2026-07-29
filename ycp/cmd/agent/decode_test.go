package main

import (
	"strings"
	"testing"

	"meko/ycp/internal/api"
)

// The agent talks to two control planes that shaped their responses differently:
// cmd/ycp returns payloads flat, api_server wraps them as {"data":…,"error":…}.
// Decoding the wrapped form straight into the payload struct silently produced
// zero values, so the agent enrolled with an empty environment_id and every
// subsequent status report was rejected with 400.
func TestDecodeBodyFlat(t *testing.T) {
	var er api.EnrollResponse
	in := `{"environment_id":"acme-prod","status_token":"mko_sts_x","mcp_base_url":"acme.mcp"}`
	if err := decodeBody(strings.NewReader(in), &er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if er.EnvironmentID != "acme-prod" || er.StatusToken != "mko_sts_x" {
		t.Fatalf("flat body not decoded: %+v", er)
	}
}

func TestDecodeBodyEnveloped(t *testing.T) {
	var er api.EnrollResponse
	in := `{"data":{"environment_id":"acme-prod","status_token":"mko_sts_x","mcp_base_url":"acme.mcp"},"error":null}`
	if err := decodeBody(strings.NewReader(in), &er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if er.EnvironmentID != "acme-prod" {
		t.Fatalf("envelope not unwrapped, environment_id = %q", er.EnvironmentID)
	}
	if er.StatusToken != "mko_sts_x" {
		t.Fatalf("envelope not unwrapped, status_token = %q", er.StatusToken)
	}
}

// An envelope carrying an error must surface it rather than look like an empty
// success — that is what made the original failure so quiet.
func TestDecodeBodyEnvelopeError(t *testing.T) {
	var er api.EnrollResponse
	in := `{"data":null,"error":{"code":"UNAUTHORIZED","message":"invalid status token"}}`
	err := decodeBody(strings.NewReader(in), &er)
	if err == nil {
		t.Fatal("expected an error from an envelope with error set")
	}
	if !strings.Contains(err.Error(), "invalid status token") {
		t.Fatalf("error message not propagated: %v", err)
	}
}

// {"data":…} with no "error" key is a payload that happens to have a data field,
// not an envelope — decode it flat.
func TestDecodeBodyDataFieldIsNotAnEnvelope(t *testing.T) {
	var out struct {
		Data string `json:"data"`
	}
	if err := decodeBody(strings.NewReader(`{"data":"literal"}`), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Data != "literal" {
		t.Fatalf("treated a plain data field as an envelope: %+v", out)
	}
}

func TestDecodeBodyEmpty(t *testing.T) {
	var er api.EnrollResponse
	if err := decodeBody(strings.NewReader(""), &er); err != nil {
		t.Fatalf("empty body should be a no-op, got %v", err)
	}
}
