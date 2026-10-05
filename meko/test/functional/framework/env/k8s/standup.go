package k8s

import (
	"context"
	"errors"
	"fmt"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
)

// StandUp builds a stack the caller keeps: the body of a setup-only preset. It
// returns the standing-env record for the registry and the teardown that reaps it.
// A later `mekoqa run --env <id>` attaches to what it built, and `mekoqa env rm`
// deletes it from the reap plan in the record.
func StandUp(ctx context.Context, cfg *core.Config) (*core.SetupResult, error) {
	secrets, err := core.NewSecrets(cfg)
	if err != nil {
		return nil, err
	}
	kc, err := FromConfig(cfg, secrets.Get)
	if err != nil {
		return nil, err
	}
	if kc.Namespace != "" {
		return nil, fmt.Errorf("env.k8s.namespace is set (%s): a preset builds a stack, it does not attach to one", kc.Namespace)
	}
	if kc.Context == "" {
		kc.Context = DiscoverContext(cfg)
	}
	if kc.Context == "" {
		return nil, errors.New("no kube context: pass --kube-context, set env.k8s.context, or select one with kubectl")
	}
	p := New(kc)
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if err := p.Prepare(ctx); err != nil {
		return nil, err
	}
	se, ok := p.StandingEnv()
	if !ok {
		_ = p.Close()
		return nil, errors.New("the stack was built but cannot be recorded")
	}
	return &core.SetupResult{Env: se, Teardown: func(context.Context) error { return p.Close() }}, nil
}
