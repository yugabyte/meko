package ampstack

import (
	"context"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/exec"
)

// TestRefreshPullSecretRemintsAndRolls: on an ampqa-managed namespace, refresh
// re-applies the docker-registry pull secret (freshly minted ECR token) and
// rolls the controller Deployment so it re-pulls immediately.
func TestRefreshPullSecretRemintsAndRolls(t *testing.T) {
	rec := newRecorder()
	rec.on("get namespace", exec.Result{Stdout: ManagedByValue}) // managed-by guard passes
	a := testApp(Config{}, rec)                                  // ECRRegistry defaulted, ecrToken faked

	if err := a.RefreshPullSecret(context.Background(), true); err != nil {
		t.Fatalf("RefreshPullSecret: %v", err)
	}
	calls := rec.snapshot()

	reappliedSecret := false
	for _, m := range appliedManifests(calls) {
		if strings.Contains(m, "kubernetes.io/dockerconfigjson") && strings.Contains(m, DefaultPullSecretName) {
			reappliedSecret = true
		}
	}
	if !reappliedSecret {
		t.Error("did not re-apply the docker-registry pull secret")
	}
	if !hasCall(calls, "patch deployment "+DefaultControllerName) {
		t.Error("did not roll (patch) the controller deployment")
	}
}

// TestRefreshPullSecretNoRollSkipsPatch: --no-roll refreshes the secret without
// touching the controller (kubelet picks the secret up on its next pull retry).
func TestRefreshPullSecretNoRollSkipsPatch(t *testing.T) {
	rec := newRecorder()
	rec.on("get namespace", exec.Result{Stdout: ManagedByValue})
	a := testApp(Config{}, rec)

	if err := a.RefreshPullSecret(context.Background(), false); err != nil {
		t.Fatalf("RefreshPullSecret: %v", err)
	}
	if hasCall(rec.snapshot(), "patch deployment") {
		t.Error("roll=false must not patch the controller deployment")
	}
}

// TestRefreshPullSecretRefusesUnmanagedNamespace: the managed-by guard prevents
// touching a namespace that isn't ampqa-owned — nothing is applied.
func TestRefreshPullSecretRefusesUnmanagedNamespace(t *testing.T) {
	rec := newRecorder()
	rec.on("get namespace", exec.Result{Stdout: "someone-else"})
	a := testApp(Config{}, rec)

	err := a.RefreshPullSecret(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "not ampqa-managed") {
		t.Fatalf("want refusal for an unmanaged namespace, got %v", err)
	}
	if hasCall(rec.snapshot(), "apply") {
		t.Error("must not apply anything to an unmanaged namespace")
	}
}

// TestRefreshPullSecretPublicImagesNoOp: with no ECR registry and no pre-encoded
// secret (public images), there is nothing to refresh and it errors clearly
// rather than silently doing nothing.
func TestRefreshPullSecretPublicImagesNoOp(t *testing.T) {
	rec := newRecorder()
	a := testApp(Config{}, rec)
	a.cfg.ECRRegistry = ""
	a.cfg.PullSecretDockerConfigB64 = ""

	if err := a.RefreshPullSecret(context.Background(), true); err == nil {
		t.Fatal("want an error when there is nothing to refresh (public images)")
	}
}

// A ghcr-only env (registry=ghcr, no ecr/quay creds) HAS a pull secret to
// refresh — the guard must not report "nothing to refresh".
func TestRefreshPullSecretGHCROnly(t *testing.T) {
	rec := newRecorder()
	rec.on("get namespace", exec.Result{Stdout: ManagedByValue}) // managed-by guard passes
	a := testApp(Config{
		Registry:        RegistryGHCR,
		ControllerImage: "amp-controller:v1",
		EndpointImage:   "amp-endpoint:v1",
	}, rec)
	a.cfg.ECRRegistry = ""
	a.cfg.PullSecretDockerConfigB64 = ""
	a.WithGHCRToken(func(context.Context) (string, error) { return "tok", nil })

	if err := a.RefreshPullSecret(context.Background(), false); err != nil {
		t.Fatalf("ghcr-only refresh should succeed, got %v", err)
	}
}

// A pure-quay stack (every image fully qualified outside ECR) must not touch
// AWS at all — the pull secret carries only the quay auth.
func TestPullSecretSkipsECRForQuayOnlyImages(t *testing.T) {
	rec := newRecorder()
	a := testApp(Config{
		ControllerImage: "quay.io/yugabyte/amp-controller:custom-20260717-9",
		EndpointImage:   "quay.io/yugabyte/amp-endpoint:custom-20260717-9",
		QuayUser:        "robot",
		QuayToken:       "tok",
	}, rec)
	minted := false
	a.WithECRToken(func(context.Context, string, string) (string, error) {
		minted = true
		return "tok", nil
	})
	if err := a.ensurePullSecret(context.Background()); err != nil {
		t.Fatalf("ensurePullSecret: %v", err)
	}
	if minted {
		t.Fatal("ECR token minted for a quay-only image set")
	}
	applied := appliedManifests(rec.snapshot())
	if len(applied) == 0 || !strings.Contains(applied[0], DefaultPullSecretName) {
		t.Fatalf("pull secret not applied: %v", applied)
	}
}

// A bare image ref resolves into ECR, so the token must still be minted.
func TestPullSecretMintsECRForECRImages(t *testing.T) {
	rec := newRecorder()
	a := testApp(Config{ControllerImage: "amp/controller:x"}, rec)
	minted := false
	a.WithECRToken(func(context.Context, string, string) (string, error) {
		minted = true
		return "tok", nil
	})
	if err := a.ensurePullSecret(context.Background()); err != nil {
		t.Fatalf("ensurePullSecret: %v", err)
	}
	if !minted {
		t.Fatal("ECR token not minted although controller image resolves to ECR")
	}
}

// The registry knob routes bare image refs: quay (default) and ghcr use the
// flat org namespaces, ecr keeps the ECR host.
func TestRegistryKnobRoutesBareRefs(t *testing.T) {
	cases := []struct {
		registry string
		want     string
	}{
		{RegistryQuay, QuayNamespace + "/amp-controller:v1"},
		{RegistryGHCR, GHCRNamespace + "/amp-controller:v1"},
		{RegistryECR, DefaultECRRegistry + "/amp-controller:v1"},
	}
	for _, tc := range cases {
		a := testApp(Config{Registry: tc.registry, ControllerImage: "amp-controller:v1"}, newRecorder())
		if got := a.images()["controller"]; got != tc.want {
			t.Errorf("registry %s: controller image = %q, want %q", tc.registry, got, tc.want)
		}
	}
}

// A ghcr image set pulls its token from the gh CLI when no explicit token is
// configured, and the pull secret carries the ghcr auth.
func TestPullSecretGHCRTokenFromGHCLI(t *testing.T) {
	rec := newRecorder()
	a := testApp(Config{
		Registry:        RegistryGHCR,
		ControllerImage: "amp-controller:v1",
		EndpointImage:   "amp-endpoint:v1",
	}, rec)
	ghCalled := false
	a.WithGHCRToken(func(context.Context) (string, error) {
		ghCalled = true
		return "gh-cli-token", nil
	})
	minted := false
	a.WithECRToken(func(context.Context, string, string) (string, error) {
		minted = true
		return "tok", nil
	})
	if err := a.ensurePullSecret(context.Background()); err != nil {
		t.Fatalf("ensurePullSecret: %v", err)
	}
	if !ghCalled {
		t.Error("gh CLI token fallback not used for ghcr images")
	}
	if minted {
		t.Error("ECR token minted for a ghcr-only image set")
	}
}

// An explicit ghcr_token wins over the gh CLI fallback.
func TestPullSecretGHCRExplicitToken(t *testing.T) {
	a := testApp(Config{
		Registry:        RegistryGHCR,
		ControllerImage: "amp-controller:v1",
		EndpointImage:   "amp-endpoint:v1",
		GHCRUser:        "bot",
		GHCRToken:       "explicit",
	}, newRecorder())
	a.WithGHCRToken(func(context.Context) (string, error) {
		t.Fatal("gh CLI must not be consulted when ghcr_token is set")
		return "", nil
	})
	if err := a.ensurePullSecret(context.Background()); err != nil {
		t.Fatalf("ensurePullSecret: %v", err)
	}
}

// The helm values (what actually lands in pod specs) must honor the registry
// knob for bare refs — this is the resolution path pods are born from, and it
// used to hardcode ECR (run #17: quay-default run pulled from ECR and died).
func TestHelmValuesFollowRegistryKnob(t *testing.T) {
	rec := newRecorder()
	a := testApp(Config{
		Registry:        RegistryQuay,
		ControllerImage: "amp-controller:custom-1",
		EndpointImage:   "amp-endpoint:custom-1",
	}, rec)
	if _, err := a.Deploy(context.Background()); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	var helmCmd string
	for _, c := range rec.snapshot() {
		joined := c.argv
		if strings.Contains(joined, "helm template") {
			helmCmd = joined
		}
	}
	if helmCmd == "" {
		t.Fatal("no helm template call recorded")
	}
	if !strings.Contains(helmCmd, "controller.image.repository="+QuayNamespace+"/amp-controller") {
		t.Errorf("controller repo not quay-resolved: %s", helmCmd)
	}
	if strings.Contains(helmCmd, DefaultECRRegistry) {
		t.Errorf("helm values leak the ECR registry on a quay run: %s", helmCmd)
	}
}
