// Package stacktest is what every suite that drives a deployed meko stack needs:
// the stack out of the test's environment, clients that fail the test on an
// internal-endpoint leak, and the stack invariants.
package stacktest

import (
	"context"
	"slices"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/llmmock"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mcpclient"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	k8senv "github.com/yugabyte/meko/meko/test/functional/framework/env/k8s"
)

const cleanupTimeout = 30 * time.Second

// Env is the test's environment as a meko stack. A provider that has none
// (the fake one) skips the test instead of failing it.
func Env(t *core.T) *k8senv.Env {
	e, ok := t.Env.(*k8senv.Env)
	if !ok || e == nil {
		t.Skipf("needs a meko stack from the k8s provider, this run's provider gave a %T", t.Env)
	}
	return e
}

// ClusterEnv is Env for a test that also inspects the cluster.
func ClusterEnv(t *core.T) *k8senv.Env {
	e := Env(t)
	if e.Stack() == nil {
		t.Skip("needs kubectl access to the stack, and this environment is a remote deployment")
	}
	return e
}

func LLM(t *core.T) *llmmock.Client {
	c := Env(t).LLM()
	if c == nil {
		t.Skip("needs the LLM mock, and this stack has none (env.mekostack.llm=off, or a stack built before llmmock)")
	}
	return c
}

// CleanupCtx is a context for deleting what a test made, independent of the
// test's own deadline, which may be the reason it is cleaning up.
func CleanupCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), cleanupTimeout)
}

// API is the stack's REST client for t. Any error it gets back that names an
// internal endpoint fails t, whatever the test was asserting (MEKO-812).
func API(t *core.T) *mekoapi.Client {
	return Env(t).API().WithErrorHook(func(e *mekoapi.Error) {
		FailOnLeak(t, e.Method+" "+e.Path, e.Message+" "+e.Body)
	})
}

// MCP is API for the MCP server, as the holder of pat (MEKO-551).
func MCP(t *core.T, pat string) *mcpclient.Client {
	return Env(t).MCP(pat, mcpclient.WithResultHook(func(tool, text string) {
		FailOnLeak(t, "mcp "+tool, text)
	}))
}

func FailOnLeak(t *core.T, where, text string) {
	if leaks := mekoapi.InternalLeaks(text); len(leaks) > 0 {
		t.Failf("%s exposes internal endpoints %v: %s", where, leaks, text)
	}
}

// HasLangfuseProject reports whether datapack id has a Langfuse project mapping.
// Without one every MCP call scoped to it fails (MEKO-621).
func HasLangfuseProject(t *core.T, stack k8senv.Stack, id string) (bool, error) {
	missing, err := stack.DatapacksWithoutLangfuseProject(t)
	if err != nil {
		return false, err
	}
	return !slices.Contains(missing, id), nil
}

// Universes is a set of extra universes the test creates in its body. They are
// removed after the test, and until then they are in the reap plan.
func Universes(t *core.T) *k8senv.Universes {
	u := Env(t).Universes(t.ResourceSuffix())
	t.OnTeardown(u.Teardown)
	return u
}

// Gateway forwards the inference gateway's port, which has no Ingress in a BYOC stack.
func Gateway(t *core.T) (string, func()) {
	kube := ClusterEnv(t).Kube()
	pf, err := kube.PortForward(context.Background(), "svc/inference-gateway-service", 0, 8088)
	if err != nil {
		t.Fatalf("forward the inference gateway: %v", err)
	}
	return "http://" + pf.LocalAddr(), func() { _ = pf.Close() }
}
