package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// What the console is allowed to do, pinned.
//
// It registers environments and observes them. It does not command them: the
// desired-state editor that briefly sat on the environment detail pane was a JSON
// textarea whose Apply REPLACED the whole resource set, and both failure modes
// landed. Opening it before the buffer had loaded and pressing Apply deleted a
// running database and mcp-server. Its "insert a Meko stack" template offered
// `<registry>/meko/meko-mcp-server:<tag>` — placeholders a browser cannot resolve —
// which one click turned into a workload that could never pull an image.
//
// Guardrails were not the fix. Registration already knows the right answer (it
// seeds desired state with the image this control plane was configured with), so
// asking a human to hand-write it was offering a choice that should not exist.
// Writing desired state stays available over the API for callers that need it.
//
// This asserts on the served asset rather than on a browser, which is the only
// level available here — there is no JS test harness in this module. It is still
// the assertion that matters: re-adding an editor means re-adding one of these
// symbols, and this test fails.
func TestConsoleExposesNoDesiredStateEditor(t *testing.T) {
	body := serveConsole(t)

	// Function names, element IDs and the button label, i.e. every surface the old
	// editor was reachable through.
	for _, forbidden := range []string{
		"openDesired",
		"applyDesired",
		"closeDesired",
		"dsModal",
		"dsText",
		"dsOpenBtn",
		"Edit desired state",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("console exposes %q — the desired-state editor is deliberately gone; "+
				"registration is how an environment is told what to run, and desired state is "+
				"writable over the API, not by hand in a browser", forbidden)
		}
	}

	// The only write the console performs. If this ever disappears the console has
	// stopped doing its one job, which the checks above would not catch.
	if !strings.Contains(body, `api("/api/v1/environments"`) {
		t.Error("console no longer posts to /api/v1/environments — registration is the surface it exists for")
	}
}

// Desired state stays visible. Removing the editor must not remove the readout:
// the declared set is what an agent was TOLD to run, and the components list is
// what it actually got up. With only the second you cannot tell "never asked for"
// from "asked for and failing".
func TestConsoleStillReadsDesiredState(t *testing.T) {
	body := serveConsole(t)

	for _, want := range []string{
		"/desired",    // GET /api/v1/environments/{id}/desired
		"desiredBox",  // where it renders
		"loadDesired", // the reader
	} {
		if !strings.Contains(body, want) {
			t.Errorf("console no longer references %q — desired state must stay readable", want)
		}
	}
}

func serveConsole(t *testing.T) string {
	t.Helper()
	return serveConsoleWith(t, "")
}

func serveConsoleWith(t *testing.T, embedSecret string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	if err := ConsoleHandler(embedSecret)(c); err != nil {
		t.Fatalf("ConsoleHandler: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("console returned %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// Off by default: the page must not carry the admin secret unless a deployment asked
// for it. The default is what a public-fronted control plane gets, and there the
// console handing its credential to every visitor would be a real exposure.
func TestConsole_DoesNotEmbedTheSecretByDefault(t *testing.T) {
	body := serveConsoleWith(t, "")
	if !strings.Contains(body, `const EMBEDDED_SECRET = "";`) {
		t.Error("default console did not render an empty embedded secret")
	}
	if !strings.Contains(body, "authModal") {
		t.Error("default console lost the in-page ask, which is how it authenticates")
	}
}

// Switched on, the secret is in the page and the console never asks.
func TestConsole_EmbedsTheSecretWhenAsked(t *testing.T) {
	body := serveConsoleWith(t, "s3cret-key")
	if !strings.Contains(body, `const EMBEDDED_SECRET = "s3cret-key";`) {
		t.Error("console did not embed the configured secret")
	}
	if strings.Contains(body, "__EMBEDDED_ADMIN_SECRET__") {
		t.Error("the placeholder survived substitution")
	}
}

// A secret containing a quote or a backslash has to survive as a JS string literal.
// Pasted in raw it would terminate the literal and break the whole script — and the
// page would still serve, so the console would just be silently dead.
func TestConsole_EmbeddedSecretIsEscaped(t *testing.T) {
	body := serveConsoleWith(t, `a"b\c`)
	if !strings.Contains(body, `const EMBEDDED_SECRET = "a\"b\\c";`) {
		t.Errorf("secret was not JSON-escaped into the page")
	}
}

// The admin-secret overlay has to actually disappear when it is hidden.
//
// `.ovl` sets `display: grid`, and an author style beats the browser's
// `[hidden] { display: none }` — so `el.hidden = true` set the attribute and left the
// dialog on screen. Entering a valid secret stored it and refreshed the fleet behind an
// overlay that never went away. A rule restoring display:none for a hidden overlay is
// the only thing that makes the attribute mean anything here.
func TestConsole_HiddenOverlayIsActuallyHidden(t *testing.T) {
	body := serveConsole(t)
	if !strings.Contains(body, ".ovl[hidden]") {
		t.Error("no .ovl[hidden] rule: setting hidden on the overlay will not hide it, " +
			"because .ovl's own display beats the UA stylesheet")
	}
	// And the overlay still has to start hidden, or it covers the console on load.
	if !strings.Contains(body, `id="authModal" hidden`) {
		t.Error("the admin-secret overlay does not start hidden")
	}
}
