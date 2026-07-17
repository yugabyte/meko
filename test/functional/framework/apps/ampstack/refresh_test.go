package ampstack

import (
	"context"
	"strings"
	"testing"

	"github.com/yugabyte/yb-amp/test/functional/framework/exec"
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
