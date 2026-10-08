package k8s

import (
	"context"
	"errors"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/llmmock"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mcpclient"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekostack"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/diag"
	"github.com/yugabyte/meko/meko/test/functional/framework/exec"
	execk8s "github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
)

// Env is a test's handle on a meko stack. Tests reach it with
// t.Env.(*k8s.Env) and use the api client, an MCP client for a PAT, and kubectl.
type Env struct {
	provider  *Provider
	app       Stack // nil for a Remote environment
	owned     bool  // a dedicated stack, torn down with the test
	backing   *backing
	api       *mekoapi.Client
	llm       *llmmock.Client // nil for a Remote environment or a stack with no mock
	apiURL    string
	mcpURL    string
	namespace string
	versions  core.DetectedVersions
}

func (e *Env) URL() string { return e.apiURL }

func (e *Env) Versions() core.DetectedVersions { return e.versions }

// Exec is nil: the stack is driven over its APIs and kubectl, not a shell.
func (e *Env) Exec() exec.RemoteExecutor { return nil }

func (e *Env) Handles() core.EnvHandles {
	ctx := ""
	if e.provider != nil {
		ctx = e.provider.cfg.Context
	}
	return core.EnvHandles{Namespace: e.namespace, Cluster: ctx, Endpoint: e.apiURL}
}

func (e *Env) Collect(ctx context.Context, into core.ArtifactSink) error {
	if e.app == nil {
		return nil
	}
	err := e.app.Collect(ctx, into)
	if b, berr := e.app.Backing(ctx); berr == nil && b.Namespace != "" && into != nil && e.provider != nil {
		kube := execk8s.New(execk8s.Config{Context: e.provider.cfg.Context, Namespace: b.Namespace})
		diag.CollectUniverseK8sDiagnostics(ctx, kube, b.Namespace, into, e.provider.logf)
	}
	return err
}

func (e *Env) Backing(ctx context.Context) (mekostack.Backing, error) {
	if e.app == nil {
		return mekostack.Backing{}, errors.New("a remote deployment has no recorded database")
	}
	return e.app.Backing(ctx)
}

// API and MCP read the stack's addresses each call: an upgrade replaces the
// pods a port-forward pointed at and the stack reconnects to the new ones.
func (e *Env) API() *mekoapi.Client {
	if e.app != nil {
		return e.app.API()
	}
	return e.api
}

func (e *Env) MCPURL() string {
	if e.app != nil {
		return e.app.MCPURL()
	}
	return e.mcpURL
}

func (e *Env) LLM() *llmmock.Client { return e.llm }

func (e *Env) MCP(pat string, opts ...mcpclient.Option) *mcpclient.Client {
	return mcpclient.New(e.MCPURL(), pat, opts...)
}

func (e *Env) Namespace() string { return e.namespace }

func (e *Env) Kube() *execk8s.Kubectl {
	if e.app == nil {
		return nil
	}
	return e.app.Kube()
}

func (e *Env) Stack() Stack { return e.app }

// GuardMark and GuardCheck make a self-built stack a core.Guarded environment.
// A remote deployment has no kubectl access and is not guarded.
func (e *Env) GuardMark(ctx context.Context) (any, error) {
	if e.app == nil {
		return nil, errors.New("no stack to guard")
	}
	return e.app.GuardMark(ctx)
}

func (e *Env) GuardCheck(ctx context.Context, mark any) ([]core.GuardFinding, error) {
	m, ok := mark.(mekostack.GuardMark)
	if e.app == nil || !ok {
		return nil, nil
	}
	fs, err := e.app.GuardCheck(ctx, m)
	if err != nil {
		return nil, err
	}
	out := make([]core.GuardFinding, len(fs))
	for i, f := range fs {
		out[i] = core.GuardFinding{Kind: f.Kind, Workload: f.Workload, Detail: f.Detail, Fatal: f.Fatal, Count: f.Count}
		if f.Route != "" {
			out[i].Matches = f.Matches
		}
	}
	return out, nil
}

// BuildStack is Provider.BuildStack for a test holding this environment.
func (e *Env) BuildStack(ctx context.Context, overrides map[string]string) (*Env, func() error, error) {
	if e.provider == nil {
		return nil, nil, errors.New("k8s: this environment has no provider to build stacks with")
	}
	return e.provider.BuildStack(ctx, overrides)
}
