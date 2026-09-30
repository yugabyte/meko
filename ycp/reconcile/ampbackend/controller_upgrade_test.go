package ampbackend

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"meko/ycp/reconcile"
)

// The endpoint image tag is not decoration on a controller upgrade — it IS the
// pin every endpoint the new controller creates runs, and what the endpoint roll
// converges the fleet onto. An upgrade that leaves it behind moves the controller
// and nothing else, and the roll that follows finds a fleet already at the old
// pin and reports success having moved nothing.
func TestControllerUpgradeMovesTheEndpointPin(t *testing.T) {
	f := &ctlServer{helm: deployedAt("0.260801.0-old"), ctlVersion: "0.260801.0-old"}
	b := f.start(t)

	mustPending(t, b.Update(t.Context(), ctlRes(1, ctlSpec(ctlVersionTarget))))

	ups := f.helm.upgrades()
	if len(ups) != 1 {
		t.Fatalf("helm upgrades = %v, want exactly one", ups)
	}
	sent := f.helm.upgradeValues(t, 0)
	for _, key := range []string{"controller.image.tag", "agent.image.tag", "controller.endpoint.image.tag"} {
		if got := valueAt(sent, key); got != ctlVersionTarget {
			t.Errorf("the upgrade does not move %s to %s: values carry %v", key, ctlVersionTarget, got)
		}
	}
}

// --reuse-values reuses the last release's COMPUTED values, so every default a
// newer chart adds arrives masked — aimed squarely at the one thing this kind
// does. The release's own user-supplied values are re-sent instead, which keeps
// the operator's overrides and lets the new chart's defaults through underneath.
func TestControllerUpgradeResendsValuesRatherThanReusingThem(t *testing.T) {
	f := &ctlServer{helm: deployedAt("0.260801.0-old"), ctlVersion: "0.260801.0-old"}
	// An override the desired state never restates, of the kind an operator
	// installs with and would lose if the upgrade dropped it.
	f.helm.values["imageRegistry"] = "quay.io/acme"
	b := f.start(t)

	mustPending(t, b.Update(t.Context(), ctlRes(1, ctlSpec(ctlVersionTarget))))

	argv := strings.Join(f.helm.upgrades()[0], " ")
	if strings.Contains(argv, "--reuse-values") {
		t.Error("the upgrade still passes --reuse-values, which masks the new chart's defaults")
	}
	if !strings.Contains(argv, "-f -") {
		t.Errorf("the upgrade passes no values document: %s", argv)
	}

	var sent map[string]any
	if err := json.Unmarshal(f.helm.upgradeStdin(0), &sent); err != nil {
		t.Fatalf("decode the values handed to helm: %v", err)
	}
	if sent["imageRegistry"] != "quay.io/acme" {
		t.Errorf("values sent = %v; an override the release carries must survive an "+
			"upgrade the desired state does not restate", sent)
	}
}

// The agent is a member of the release it upgrades, so its own rollout kills the
// helm client — and if that lands mid-submit the release sits in pending-upgrade
// with the NEW values already recorded. The values comparison then short-circuits
// and the rollout check finds the OLD pod template, so without reading the status
// the tick reports Pending forever with nothing saying why.
func TestControllerUpdateReportsAReleaseStillInFlight(t *testing.T) {
	f := &ctlServer{helm: deployedAt(ctlVersionTarget), ctlVersion: ctlVersionTarget}
	f.helm.status = "pending-upgrade"
	b := f.start(t)

	err := b.Update(t.Context(), ctlRes(1, ctlSpec(ctlVersionTarget)))
	mustPending(t, err)
	if !strings.Contains(err.Error(), "pending-upgrade") {
		t.Errorf("error = %v, want it to name the release's state", err)
	}
	if got := f.helm.upgrades(); len(got) != 0 {
		t.Errorf("ran %v while helm had an operation in flight; helm refuses a "+
			"concurrent one, so this is a failed verb every tick", got)
	}
}

// A failed release is reported with helm's own description and the command that
// usually recovers it, rather than being retried blind into the same failure.
// Retryable, not permanent: a failed release is upgradable, and the engine's
// backoff is the right cadence.
func TestControllerUpdateReportsAFailedRelease(t *testing.T) {
	f := &ctlServer{helm: deployedAt(ctlVersionTarget), ctlVersion: ctlVersionTarget}
	f.helm.status = "failed"
	b := f.start(t)

	err := b.Update(t.Context(), ctlRes(1, ctlSpec(ctlVersionTarget)))
	if err == nil {
		t.Fatal("a failed release reported success")
	}
	var perm *reconcile.PermanentError
	if errors.As(err, &perm) {
		t.Error("a failed release is Permanent; it is upgradable, so it should back off and retry")
	}
	if !strings.Contains(err.Error(), "helm rollback") {
		t.Errorf("error = %v, want it to name the command that recovers a failed release", err)
	}
}

// A release status this helm cannot report must not wedge the kind: an older
// helm has no `status --output json`, and refusing to upgrade over a cosmetic
// gap would strand the controller.
func TestControllerUpdateProceedsWhenStatusIsUnreadable(t *testing.T) {
	f := &ctlServer{helm: deployedAt("0.260801.0-old"), ctlVersion: "0.260801.0-old"}
	f.helm.status = "not json at all"
	b := f.start(t)

	mustPending(t, b.Update(t.Context(), ctlRes(1, ctlSpec(ctlVersionTarget))))
	if got := f.helm.upgrades(); len(got) != 1 {
		t.Errorf("helm upgrades = %v, want one: an unreadable status is not a reason to refuse", got)
	}
}

// helm ignores --version against a packaged chart rather than refusing it, so a
// declaration that disagrees with the archive loops forever without this: upgrade,
// deploy the archive's own version, see a mismatch, upgrade again. The archive URL
// is how this kind is exercised before the chart is published, so the trap is on
// the only path anyone can use today.
func TestControllerChartVersionAgainstAnArchiveIsPermanent(t *testing.T) {
	f := &ctlServer{helm: deployedAt(ctlVersionTarget), ctlVersion: ctlVersionTarget,
		chart: "https://uploads.dev.yugabyte.com/dshubin/amp-controller-chart/yb-amp-controller-0.260901.0.tgz"}
	f.helm.chartVersion = "0.260901.0"
	b := f.start(t)

	err := b.Update(t.Context(), ctlRes(1, ctlSpecChart(ctlVersionTarget, "0.260902.0")))
	var perm *reconcile.PermanentError
	if !errors.As(err, &perm) {
		t.Fatalf("Update = %v, want Permanent: no retry changes what is inside the .tgz", err)
	}
	if got := f.helm.upgrades(); len(got) != 0 {
		t.Errorf("ran %v; issuing it is what makes the loop", got)
	}
}

// The same declaration against a repository reference is honoured, so the guard
// above is about the archive and not about chart_version.
func TestControllerChartVersionAgainstARepoIsHonoured(t *testing.T) {
	f := &ctlServer{helm: deployedAt(ctlVersionTarget), ctlVersion: ctlVersionTarget}
	f.helm.chartVersion = "0.260901.0"
	b := f.start(t)

	mustPending(t, b.Update(t.Context(), ctlRes(1, ctlSpecChart(ctlVersionTarget, "0.260902.0"))))
	argv := strings.Join(f.helm.upgrades()[0], " ")
	if !strings.Contains(argv, "--version 0.260902.0") {
		t.Errorf("helm argv does not carry the declared chart version: %s", argv)
	}
}

// ctlSpecChart is ctlSpec with a declared chart version.
func ctlSpecChart(version, chartVersion string) string {
	return `{"version":"` + version + `","chart_version":"` + chartVersion + `"}`
}
