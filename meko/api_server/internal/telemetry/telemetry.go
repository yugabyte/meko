// Backend Rudderstack emitter. api_server owns the events that describe
// server-observable side effects (a datapack created, a KB doc completed
// processing, an invite dispatched) so every entry path — UI, MCP tools,
// direct API — contributes to the same funnel. The UI keeps events it
// alone can observe (auth clicks, invite accepts, api-key generation).
//
// Failures are non-fatal: a rudderstack outage never breaks a user
// request; the Warn on write and the noop client on missing config
// mirror the sharing_service SES pattern (nil-safe optional dep).

package telemetry

import (
	"context"
)

// Event names — kept as constants so callers and the meko_ui events.ts
// registry stay verifiable side-by-side. Names are snake_case per the
// existing GTM convention. document_uploaded and conversation_started
// still fire from meko_ui only; document_uploaded's three upload paths
// (CreateKB new-source, CreateKB completed-source sweep, UploadFile)
// aren't yet unified on a service seam that would let BE emit once per
// doc without duplicates, and conversation_started's row is minted in
// the Python inference gateway.
const (
	EventDatapackCreated = "datapack_created"
	EventDatapackShared  = "datapack_shared"
)

// Source labels for the `source` property Growth uses to slice a funnel
// by origin. Every server-emitted event carries one.
//
// SourceProvisioner tags the internal Create fired by
// ProvisioningService.ProvisionNewAccount for the default datapack — a
// server-authored artefact the funnel must exclude (every new signup
// would otherwise land a phantom datapack_created).
const (
	SourceUI          = "ui"
	SourceMCP         = "mcp"
	SourceAPI         = "api"
	SourceAdmin       = "admin"
	SourceProvisioner = "provisioner"
)

// Auth method constants mirror internal/auth's ContextKeyAuthMethod
// values so this package can map auth type → source without a reverse
// dependency into auth. Kept as untyped strings; the auth package is
// the authority on the values and holds its own copy.
const (
	authMethodCognito = "cognito"
	authMethodPAT     = "pat"
)

// Client-hint values accepted on the X-Meko-Client HTTP header. A PAT
// call from meko-mcp-server carries "mcp" so its events land in the MCP
// slice of the funnel instead of the generic API bucket.
const (
	ClientHintMCP   = "mcp"
	ClientHintAdmin = "admin"
	ClientHintUI    = "ui"
)

// SourceFromAuthMethod derives a Rudderstack `source` from the auth
// context. The client hint is checked FIRST — meko-mcp-server's MCP
// tools swap their fastmcp OAuth-proxy token for the user's upstream
// Cognito access token before calling api_server (see
// meko-mcp-server/src/tools.py:_get_meko_auth), so a Cognito JWT alone
// cannot distinguish a browser session from an MCP tool call. Without
// the hint-first check those MCP-via-Cognito events landed as
// source=ui, the service's UI-skip gate fired, and the whole MCP
// funnel went dark (the exact gap this PR is meant to close).
//
// With the hint present (`X-Meko-Client: mcp|admin`) the explicit tag
// wins over the auth method; without a hint, Cognito ⇒ UI (browser),
// PAT ⇒ API (generic script / CLI). Empty method falls back to API
// so an unauthenticated internal caller does not attribute to a real
// user cohort.
func SourceFromAuthMethod(authMethod, clientHint string) string {
	switch clientHint {
	case ClientHintMCP:
		return SourceMCP
	case ClientHintAdmin:
		return SourceAdmin
	case ClientHintUI:
		return SourceUI
	}
	if authMethod == authMethodCognito {
		return SourceUI
	}
	return SourceAPI
}

// Client is the injectable surface every service uses. Two impls:
// RudderstackClient (real network writes) and NoopClient (tests + any
// env without a write-key). Callers never branch on nil — the router
// hands out a Noop when config is missing.
type Client interface {
	// Track records an event for the given user. Failures are logged
	// but never returned as errors to the caller — telemetry must not
	// block the request path.
	Track(ctx context.Context, userID, event string, props map[string]any)
	// Identify attaches persistent traits to a user. Currently unused
	// by BE (the UI does the identify() on sign-in so anonymous_id
	// carries through), reserved for admin-created accounts.
	Identify(ctx context.Context, userID string, traits map[string]any)
	// Flush drains the batch queue. Call once at server shutdown so a
	// clean stop does not lose the tail.
	Flush(ctx context.Context) error
}
