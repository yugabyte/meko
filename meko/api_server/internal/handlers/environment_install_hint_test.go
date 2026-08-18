package handlers

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func hintFor(t *testing.T, kind, path, host string, headers map[string]string) string {
	t.Helper()
	req := httptest.NewRequest("POST", path, nil)
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c := echo.New().NewContext(req, httptest.NewRecorder())
	return installHint(c, kind, "acme-prod", "ycp_enr_deadbeef")
}

// The hint is the only place an operator is told how to attach an agent, so it has to
// be runnable as printed. These are the specific ways the old one was not.
func TestInstallHint_IsRunnableAsPrinted(t *testing.T) {
	hint := hintFor(t, "meko", "/api/v1/environments", "ycp.example.com", nil)

	for _, want := range []string{
		// The token, in a Secret rather than on a command line.
		"--from-literal=enrollment-token=ycp_enr_deadbeef",
		// The chart that actually exists: a component of `meko` in meko-devops.
		"helm upgrade --install meko-agent charts/meko",
		// values-agent.yaml is what suppresses the other seven components. Without it
		// a data-plane install puts an api-server and a Langfuse in someone's cluster.
		"-f charts/meko/values-agent.yaml",
		// Both required values, filled in rather than left as placeholders.
		"--set agent.ycpUrl=http://ycp.example.com",
		"--set agent.envName=acme-prod",
		// The one thing the server genuinely cannot know.
		"--set-string agent.image.tag=",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint is missing %q:\n%s", want, hint)
		}
	}

	// The old hint's placeholders. Their absence is the point of this test.
	for _, unwanted := range []string{"<ns>", "<this control plane>", "--set agent.enabled=true"} {
		if strings.Contains(hint, unwanted) {
			t.Errorf("hint still contains the placeholder %q:\n%s", unwanted, hint)
		}
	}
}

// An amp environment gets a different hint: it installs the amp controller chart
// with its reconcile-agent turned on (the agent is the sole YCP client), not the
// meko-agent. It must be runnable as printed and must not leak the meko chart into
// an amp install.
func TestInstallHint_Amp(t *testing.T) {
	hint := hintFor(t, "amp", "/api/v1/environments", "ycp.example.com", nil)

	for _, want := range []string{
		// The token, in a Secret the agent reads from — the chart's default name.
		"--from-literal=enrollment-token=ycp_enr_deadbeef",
		"amp-reconcile-agent-enrollment",
		// The amp controller chart, with the reconcile-agent turned on and pointed here.
		"helm upgrade --install amp-controller amp/deploy/helm/yb-amp-controller",
		"--set agent.enabled=true",
		"--set agent.ycpUrl=http://ycp.example.com",
		"--set agent.envName=acme-prod",
		"--set agent.enrollTokenSecret.name=amp-reconcile-agent-enrollment",
		// The YugabyteDB prerequisite is called out.
		"YugabyteDB",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("amp hint is missing %q:\n%s", want, hint)
		}
	}

	// The retired controller-side YCP loop must not reappear, and the meko-agent
	// install must not bleed into an amp hint.
	for _, unwanted := range []string{
		"controller.ycp",                // the controller no longer talks to YCP
		"amp-controller-ycp-enrollment", // renamed to the agent's secret
		"meko-agent", "values-agent.yaml",
	} {
		if strings.Contains(hint, unwanted) {
			t.Errorf("amp hint contains retired/foreign string %q:\n%s", unwanted, hint)
		}
	}
}

// A YCP mounted under a subpath has to print its own URL including that subpath —
// production serves it at https://devcloud.yugabyte.com/ycp/, and an agent pointed at
// the bare domain gets 404s from every call.
func TestInstallHint_CarriesTheBasePath(t *testing.T) {
	hint := hintFor(t, "meko", "/ycp/api/v1/environments", "devcloud.yugabyte.com", map[string]string{
		"X-Forwarded-Proto": "https",
	})
	if !strings.Contains(hint, "--set agent.ycpUrl=https://devcloud.yugabyte.com/ycp") {
		t.Errorf("hint did not carry scheme + base path:\n%s", hint)
	}
}

// Root-mounted YCP: no prefix invented.
func TestInstallHint_NoBasePath(t *testing.T) {
	hint := hintFor(t, "meko", "/api/v1/environments", "ycp.internal", nil)
	if !strings.Contains(hint, "--set agent.ycpUrl=http://ycp.internal \\") {
		t.Errorf("hint should carry a bare host with no trailing path:\n%s", hint)
	}
}

// Behind a TLS-terminating proxy the request arrives as plain HTTP. Printing an
// http:// URL for an https:// control plane sends the agent somewhere that redirects
// or refuses, so the forwarded headers win.
func TestInstallHint_HonorsForwardedHeaders(t *testing.T) {
	hint := hintFor(t, "meko", "/api/v1/environments", "10.0.0.5:25433", map[string]string{
		"X-Forwarded-Proto": "https, http",
		"X-Forwarded-Host":  "ycp.example.com, internal",
	})
	if !strings.Contains(hint, "--set agent.ycpUrl=https://ycp.example.com") {
		t.Errorf("forwarded scheme/host not used:\n%s", hint)
	}
	if strings.Contains(hint, "10.0.0.5") {
		t.Errorf("hint leaked the internal address:\n%s", hint)
	}
}
