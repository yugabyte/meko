// Package k8s is the environment provider that builds meko stacks on a
// Kubernetes cluster. A SingleStack test shares one run-scoped stack in
// mekoqa-st-<run>, each DedicatedStack test gets a stack of its own in
// mekoqa-ds-<run>-<n> that is built and torn down with it, a Remote test
// attaches to a deployment by URL, and a ClusterOnly test gets no stack, only
// the cluster and the database provider to build its own. With
// env.k8s.namespace set the run attaches to a stack that already runs there and
// builds nothing.
//
// Every stack is one mekostack.App. This package decides which namespace a test
// gets, when the stack is built and who tears it down.
package k8s

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"maps"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/cognitomock"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/llmmock"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekoapi"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekostack"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/env/reap"
	execk8s "github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
	"github.com/yugabyte/meko/meko/test/functional/framework/universe"
)

const (
	sharedPrefix    = "mekoqa-st-"
	dedicatedPrefix = "mekoqa-ds-"
)

type SecretResolver func(name string) (value string, ok bool, err error)

type Config struct {
	Context   string
	Namespace string // attach to the stack running here
	RunID     string

	RemoteURL    string
	RemoteMCPURL string

	Stack         mekostack.Config
	DeployTimeout time.Duration

	// The run config and secret resolver Stack came from, so a test can build a
	// stack of its own from the same config with overrides.
	core    *core.Config
	resolve SecretResolver
}

// FromConfig reads the provider's configuration. resolve supplies the Quay
// credential from the secrets section, with the plain knobs as the fallback. The
// kube context is left as configured: the caller fills in the ambient one.
func FromConfig(c *core.Config, resolve SecretResolver) (Config, error) {
	stack, err := mekostack.FromConfig(c)
	if err != nil {
		return Config{}, err
	}
	user, token, err := quayCredential(c, resolve)
	if err != nil {
		return Config{}, err
	}
	stack.QuayUser, stack.QuayToken = user, token
	stack.Owner = c.GetString("owner", "")

	runID := RunIDFrom(c.GetString("report.portal.run_id", ""))
	stack.RunID = runID
	if out := c.GetString("report.out_dir", ""); out != "" {
		if stack.ValuesDir == "" {
			stack.ValuesDir = filepath.Join(out, runID, "mekostack")
		}
		if stack.DiagDir == "" {
			stack.DiagDir = filepath.Join(out, runID, "diagnostics")
		}
	}
	if c.GetString("report.collect_diagnostics", core.DefaultCollectDiagnostics) == "never" {
		stack.DiagDir = ""
	}
	return Config{
		Context:       c.GetString("env.k8s.context", ""),
		Namespace:     c.GetString("env.k8s.namespace", ""),
		RunID:         runID,
		RemoteURL:     c.GetString("env.k8s.remote_url", ""),
		RemoteMCPURL:  c.GetString("env.k8s.remote_mcp_url", ""),
		Stack:         stack,
		DeployTimeout: c.GetDuration("env.mekostack.timeouts.deploy", 0),
		core:          c,
		resolve:       resolve,
	}, nil
}

func quayCredential(c *core.Config, resolve SecretResolver) (user, token string, err error) {
	name := c.GetString("env.mekostack.quay_secret", "")
	if resolve != nil && name != "" {
		v, ok, err := resolve(name)
		if err != nil {
			return "", "", fmt.Errorf("secret %q: %w", name, err)
		}
		if ok {
			u, t, found := strings.Cut(strings.TrimSpace(v), ":")
			if !found || u == "" || t == "" {
				return "", "", fmt.Errorf("secret %q must be \"<user>:<token>\"", name)
			}
			return u, t, nil
		}
	}
	return c.GetString("env.mekostack.quay_user", ""), c.GetString("env.mekostack.quay_token", ""), nil
}

// RunIDFrom makes a namespace-safe run id: lowercase alphanumerics, at most ten,
// from the portal's run id when there is one, else random.
func RunIDFrom(portalRunID string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(portalRunID) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
		if b.Len() == 10 {
			break
		}
	}
	if b.Len() > 0 {
		return b.String()
	}
	var raw [4]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

func SharedNamespace(runID string) string { return sharedPrefix + runID }

func DedicatedNamespace(runID string, n int64) string {
	return fmt.Sprintf("%s%s-%d", dedicatedPrefix, runID, n)
}

// DiscoverContext is the kube context a run builds on: env.k8s.context, else the
// ambient current context. "" when neither exists.
func DiscoverContext(c *core.Config) string {
	if ctx := c.GetString("env.k8s.context", ""); ctx != "" {
		return ctx
	}
	cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "kubectl", "config", "current-context").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Stack is one deployed stack as the provider and its environments use it.
// *mekostack.App implements it.
type Stack interface {
	Deploy(ctx context.Context) error
	Attach(ctx context.Context) error
	Teardown(ctx context.Context) error
	Close()
	Collect(ctx context.Context, into apps.ArtifactSink) error
	NamespaceGone(ctx context.Context) (bool, error)
	Namespace() string
	APIURL() string
	MCPURL() string
	LLM() *llmmock.Client
	API() *mekoapi.Client
	Kube() *execk8s.Kubectl
	Workloads(ctx context.Context) ([]mekostack.Workload, error)
	Extensions(ctx context.Context) ([]string, error)
	DatapacksWithoutLangfuseProject(ctx context.Context) ([]string, error)
	Upgrade(ctx context.Context, to mekostack.Release) error
	ProbeAPI(ctx context.Context) error
	GuardMark(ctx context.Context) (mekostack.GuardMark, error)
	GuardCheck(ctx context.Context, m mekostack.GuardMark) ([]mekostack.Finding, error)
	AuthMode() mekostack.AuthMode
	NewAccounts(ctx context.Context, prefix string, n int) ([]mekostack.Account, error)
	CognitoControl(ctx context.Context) (*cognitomock.Client, error)
	Versions(ctx context.Context) (product, database string)
	Backing(ctx context.Context) (mekostack.Backing, error)
	YSQL(ctx context.Context, query string) (string, error)
}

type stack struct {
	app     Stack
	owned   bool
	backing *backing
}

type Provider struct {
	cfg  Config
	logf func(string, ...any)

	newStack   func(mekostack.Config) Stack
	newCreator func(prov universe.Provider, tag string) (creator, error)
	yba        *ybaRun

	mu           sync.Mutex
	shared       *stack
	sharedGen    int                      // bumped by every rebuild of the run's stack, names its namespace
	rebuilt      chan struct{}            // closed when the rebuild in flight ends, nil when none is
	live         map[string]struct{}      // namespaces of every stack this provider stands up
	extra        map[string][]reap.NSReap // what else each stack namespace needs reaped, its backing
	onPlanChange func()

	counter atomic.Int64
}

func New(cfg Config) *Provider {
	return &Provider{
		cfg:      cfg,
		logf:     log.Printf,
		newStack: func(c mekostack.Config) Stack { return mekostack.New(c) },
		live:     map[string]struct{}{},
		extra:    map[string][]reap.NSReap{},
	}
}

func (p *Provider) Attaching() bool { return p.cfg.Namespace != "" }

// Validate checks what can be known before anything is created. A run that
// attaches builds nothing, so it has nothing to check.
func (p *Provider) Validate() error {
	if p.Attaching() {
		return nil
	}
	return errors.Join(p.stackConfig("", nil).Validate(), p.validateDatabase())
}

func (p *Provider) SharesInfraAcrossTests() bool { return true }

func (p *Provider) OnReapPlanChange(fn func()) {
	p.mu.Lock()
	p.onPlanChange = fn
	p.mu.Unlock()
}

func (p *Provider) log(format string, args ...any) {
	if p.logf != nil {
		p.logf(format, args...)
	}
}

func (p *Provider) stackConfig(ns string, b *mekostack.Backing) mekostack.Config {
	c := p.cfg.Stack
	c.Context = p.cfg.Context
	c.Namespace = ns
	c.Backing = b
	return c
}

// dedicatedStackConfig is stackConfig with the test's BackingConfig laid over the run config, so
// a test can set the stack's own knobs as well as its database.
func (p *Provider) dedicatedStackConfig(spec core.EnvSpec, ns string) (mekostack.Config, error) {
	if spec.BackingConfig == nil || p.cfg.core == nil {
		return p.stackConfig(ns, nil), nil
	}
	o := spec.BackingConfig(p.cfg.core)
	if len(o) == 0 {
		return p.stackConfig(ns, nil), nil
	}
	withRun := map[string]string{"report.portal.run_id": p.cfg.RunID}
	maps.Copy(withRun, o)
	c, err := FromConfig(p.cfg.core.Derive(withRun), p.cfg.resolve)
	if err != nil {
		return mekostack.Config{}, err
	}
	sc := c.Stack
	sc.Context, sc.Namespace = p.cfg.Context, ns
	return sc, nil
}

func (p *Provider) track(ns string, live bool) {
	p.mu.Lock()
	if live {
		p.live[ns] = struct{}{}
	} else {
		delete(p.live, ns)
	}
	fn := p.onPlanChange
	p.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// Prepare stands the run-scoped stack up before the scheduler starts, so the
// first test's provision deadline does not have to absorb the bring-up. With
// env.k8s.namespace set it attaches instead. A failed bring-up is torn down.
func (p *Provider) Prepare(ctx context.Context) error {
	p.mu.Lock()
	have := p.shared != nil
	p.mu.Unlock()
	if have {
		return nil
	}
	attach := p.Attaching()
	ns := p.cfg.Namespace
	var bk *backing
	var sb *mekostack.Backing
	if !attach {
		p.mu.Lock()
		gen := p.sharedGen
		p.mu.Unlock()
		ns = SharedNamespace(p.cfg.RunID)
		tag := p.cfg.RunID
		if gen > 0 {
			ns = fmt.Sprintf("%s-r%d", ns, gen)
			tag = fmt.Sprintf("%s-r%d", tag, gen)
		}
		var err error
		if bk, sb, err = p.prepareBacking(ctx, core.EnvSpec{}, true, tag, ns, p.cfg.Stack.DatabaseImage); err != nil {
			return err
		}
	}
	app := p.newStack(p.stackConfig(ns, sb))

	dctx := ctx
	if p.cfg.DeployTimeout > 0 {
		var cancel context.CancelFunc
		dctx, cancel = context.WithTimeout(ctx, p.cfg.DeployTimeout)
		defer cancel()
	}
	if attach {
		p.log("k8s: attaching to the stack in %s", ns)
		if err := app.Attach(dctx); err != nil {
			app.Close()
			return fmt.Errorf("attach to the stack in %s: %w", ns, err)
		}
	} else {
		p.log("k8s: building the run's stack in %s", ns)
		p.track(ns, true)
		if err := app.Deploy(dctx); err != nil {
			p.teardown(app, ns, bk)
			return err
		}
	}
	p.mu.Lock()
	p.shared = &stack{app: app, owned: !attach, backing: bk}
	p.mu.Unlock()
	return nil
}

// RebuildSharedInfra replaces the run's stack with a new one in a fresh namespace
// on a fresh database, so the tests after one that broke it, or after a failed
// bring-up, still run.
func (p *Provider) RebuildSharedInfra(ctx context.Context) error {
	if p.Attaching() {
		return core.ErrNoSharedInfraRebuild
	}
	done := make(chan struct{})
	p.mu.Lock()
	old := p.shared
	p.shared = nil
	p.sharedGen++
	p.rebuilt = done
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.rebuilt = nil
		p.mu.Unlock()
		close(done)
	}()
	if old != nil {
		old.app.Close()
		if err := p.teardown(old.app, old.app.Namespace(), old.backing); err != nil {
			p.log("k8s: tearing down the broken stack: %v", err)
		}
	}
	return p.Prepare(ctx)
}

// sharedStack is the run's stack, waiting out a rebuild in flight: a worker
// freed mid-rebuild would otherwise fail its test for want of a stack.
func (p *Provider) sharedStack(ctx context.Context) (*stack, error) {
	p.mu.Lock()
	st, wait := p.shared, p.rebuilt
	p.mu.Unlock()
	if st == nil && wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, fmt.Errorf("k8s: waiting for the run's stack to be rebuilt: %w", ctx.Err())
		}
		p.mu.Lock()
		st = p.shared
		p.mu.Unlock()
	}
	if st == nil {
		return nil, errors.New("k8s: the run's stack was not prepared")
	}
	return st, nil
}

// SharedInfraHealthy waits, within ctx, for the run's stack to answer its API.
func (p *Provider) SharedInfraHealthy(ctx context.Context) error {
	p.mu.Lock()
	st := p.shared
	p.mu.Unlock()
	if st == nil {
		return errors.New("k8s: the run's stack is gone")
	}
	var last error
	for {
		pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		last = st.app.ProbeAPI(pctx)
		cancel()
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the stack in %s does not serve: %w", st.app.Namespace(), last)
		case <-time.After(5 * time.Second):
		}
	}
}

// teardown deletes a stack on its own deadline, which a cancelled run context
// does not shorten, then its backing.
func (p *Provider) teardown(app Stack, ns string, b *backing) error {
	budget := p.cfg.DeployTimeout
	if budget <= 0 {
		budget = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	var errs []error
	if err := app.Teardown(ctx); err != nil {
		p.log("k8s: tearing down %s: %v", ns, err)
		errs = append(errs, err)
	} else {
		p.track(ns, false)
	}
	if err := p.teardownBacking(b); err != nil {
		p.log("k8s: tearing down the backing of %s: %v", ns, err)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (p *Provider) Provision(ctx context.Context, spec core.EnvSpec, seed *core.Seed) (core.Environment, error) {
	switch spec.Shape {
	case core.SingleStack:
		st, err := p.sharedStack(ctx)
		if err != nil {
			return nil, err
		}
		return p.newEnv(ctx, st.app, false), nil
	case core.DedicatedStack:
		n := p.counter.Add(1)
		ns := DedicatedNamespace(p.cfg.RunID, n)
		sc, err := p.dedicatedStackConfig(spec, ns)
		if err != nil {
			return nil, err
		}
		bk, sb, err := p.prepareBacking(ctx, spec, false, fmt.Sprintf("%s-%d", p.cfg.RunID, n), ns, sc.DatabaseImage)
		if err != nil {
			return nil, err
		}
		sc.Backing = sb
		app := p.newStack(sc)
		p.log("k8s: building a dedicated stack in %s", ns)
		p.track(ns, true)
		if err := app.Deploy(ctx); err != nil {
			p.teardown(app, ns, bk)
			return nil, err
		}
		env := p.newEnv(ctx, app, true)
		env.backing = bk
		return env, nil
	case core.ClusterOnly:
		return &Env{provider: p}, nil
	case core.Remote:
		if p.cfg.RemoteURL == "" {
			return nil, &core.UnsupportedShapeError{Provider: "k8s", Shape: core.Remote, Why: "env.k8s.remote_url is not set"}
		}
		return &Env{
			provider: p, api: mekoapi.New(p.cfg.RemoteURL), apiURL: p.cfg.RemoteURL, mcpURL: p.cfg.RemoteMCPURL,
		}, nil
	}
	return nil, &core.UnsupportedShapeError{Provider: "k8s", Shape: spec.Shape, Why: "unknown shape"}
}

func (p *Provider) newEnv(ctx context.Context, app Stack, owned bool) *Env {
	e := &Env{provider: p, app: app, owned: owned, apiURL: app.APIURL(), mcpURL: app.MCPURL(), api: app.API(), llm: app.LLM(), namespace: app.Namespace()}
	product, db := app.Versions(ctx)
	e.versions = core.DetectedVersions{Product: versionOrEmpty(product), YB: versionOrEmpty(db)}
	return e
}

// versionOrEmpty drops a tag that is not a version (custom-<sha>), which version
// gating reads as "unknown, do not enforce".
func versionOrEmpty(tag string) string {
	if tag == "" || tag[0] < '0' || tag[0] > '9' {
		return ""
	}
	return tag
}

// Teardown ends a test's environment. The run-scoped stack outlives its tests, so
// only a dedicated stack goes here.
// BuildStack stands up a stack of the test's own in a fresh dedicated namespace,
// from the run config with overrides laid over it (e.g. an older chart and
// image version for an upgrade test). It is in the reap plan until the
// returned teardown runs.
func (p *Provider) BuildStack(ctx context.Context, overrides map[string]string) (*Env, func() error, error) {
	if p.cfg.core == nil {
		return nil, nil, errors.New("k8s: this provider was not built from a run config")
	}
	// Without a portal run id FromConfig draws a fresh random one, and the
	// stack's labels and values dir would name a run other than its namespace.
	withRun := map[string]string{"report.portal.run_id": p.cfg.RunID}
	maps.Copy(withRun, overrides)
	c, err := FromConfig(p.cfg.core.Derive(withRun), p.cfg.resolve)
	if err != nil {
		return nil, nil, err
	}
	n := p.counter.Add(1)
	ns := DedicatedNamespace(p.cfg.RunID, n)
	sc := c.Stack
	sc.Context = p.cfg.Context
	sc.Namespace = ns
	backingFrom := core.EnvSpec{BackingConfig: func(*core.Config) map[string]string { return overrides }}
	bk, sb, err := p.prepareBacking(ctx, backingFrom, false, fmt.Sprintf("%s-%d", p.cfg.RunID, n), ns, sc.DatabaseImage)
	if err != nil {
		return nil, nil, err
	}
	sc.Backing = sb
	app := p.newStack(sc)
	p.log("k8s: building a stack of the test's own in %s", ns)
	p.track(ns, true)
	if err := app.Deploy(ctx); err != nil {
		p.teardown(app, ns, bk)
		return nil, nil, err
	}
	env := p.newEnv(ctx, app, true)
	env.backing = bk
	return env, func() error { return p.teardown(app, ns, bk) }, nil
}

func (p *Provider) Teardown(_ context.Context, env core.Environment) error {
	e, ok := env.(*Env)
	if !ok {
		return fmt.Errorf("k8s teardown: unexpected environment type %T", env)
	}
	if !e.owned || e.app == nil {
		return nil
	}
	e.app.Close()
	return p.teardown(e.app, e.namespace, e.backing)
}

// Attach is unsupported: a standing stack is attached through env.k8s.namespace,
// and `mekoqa env rm` reaps it from the reap plan its record carries. Failing
// here keeps an entry with no plan from looking torn down when nothing was.
func (p *Provider) Attach(context.Context, string) (core.Environment, error) {
	return nil, errors.New("k8s: attach by reference is not supported (use env.k8s.namespace to run against a stack, `mekoqa env rm` to reap one)")
}

// Close releases the run's resources: the run-scoped stack is deleted when the
// provider built it, and only its forwards are closed when it attached.
func (p *Provider) Close() error {
	p.mu.Lock()
	st := p.shared
	p.shared = nil
	p.mu.Unlock()
	var err error
	switch {
	case st == nil:
	case !st.owned:
		st.app.Close()
	default:
		st.app.Close()
		err = p.teardown(st.app, st.app.Namespace(), st.backing)
	}
	return errors.Join(err, p.closeYBA())
}

// StandingEnv records the run's stack for `run --env-mode keep` and
// `--setup-only`. The process is about to exit, so its port-forwards are closed:
// they are local processes that would otherwise outlive it. Only a stack the
// provider built is recordable.
func (p *Provider) StandingEnv() (*core.StandingEnv, bool) {
	p.mu.Lock()
	st := p.shared
	p.mu.Unlock()
	if st == nil || !st.owned {
		return nil, false
	}
	st.app.Close()
	ns := st.app.Namespace()
	kept := p.planFor([]string{ns})
	// The kept stack's universe needs the run's YBA, so the YBA is kept with it.
	p.mu.Lock()
	kept.Namespaces = append(kept.Namespaces, p.extra[ybaPlanKey]...)
	p.mu.Unlock()
	plan, _ := kept.JSON()
	return &core.StandingEnv{
		Name:     ns,
		Shape:    core.SingleStack.String(),
		Provider: "k8s",
		Info: map[string]string{
			"context":   p.cfg.Context,
			"namespace": ns,
			"reap_plan": plan,
			"inspect":   fmt.Sprintf("kubectl --context %s -n %s get pods", p.cfg.Context, ns),
		},
	}, true
}

func (p *Provider) planFor(namespaces []string) *reap.Plan {
	plan := &reap.Plan{}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ns := range namespaces {
		plan.Namespaces = append(plan.Namespaces, reap.NSReap{
			Kind: reap.KindMeko, KubeContext: p.cfg.Context, Namespace: ns, HelmRelease: p.cfg.Stack.Release,
		})
		plan.Namespaces = append(plan.Namespaces, p.extra[ns]...)
	}
	return plan
}

// ReapPlan includes a backing whose stack is already gone.
func (p *Provider) ReapPlan() *reap.Plan {
	p.mu.Lock()
	var nss []string
	for ns := range p.live {
		nss = append(nss, ns)
	}
	var orphans []string
	for ns := range p.extra {
		if _, live := p.live[ns]; !live {
			orphans = append(orphans, ns)
		}
	}
	p.mu.Unlock()
	if len(nss) == 0 && len(orphans) == 0 {
		return nil
	}
	sort.Strings(nss)
	sort.Strings(orphans)
	plan := p.planFor(nss)
	p.mu.Lock()
	for _, ns := range orphans {
		plan.Namespaces = append(plan.Namespaces, p.extra[ns]...)
	}
	p.mu.Unlock()
	return plan
}

func (p *Provider) SharedInfraGone(ctx context.Context) (string, bool) {
	p.mu.Lock()
	st := p.shared
	p.mu.Unlock()
	if st == nil {
		return "", false
	}
	gone, err := st.app.NamespaceGone(ctx)
	if err != nil || !gone {
		return "", false
	}
	return "namespace " + st.app.Namespace(), true
}

var _ Stack = (*mekostack.App)(nil)
