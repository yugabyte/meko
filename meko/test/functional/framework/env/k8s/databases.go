package k8s

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mekostack"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/yugabytedk8s"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/diag"
	"github.com/yugabyte/meko/meko/test/functional/framework/env/reap"
	execk8s "github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
	"github.com/yugabyte/meko/meko/test/functional/framework/universe"
)

const (
	magePreview   = "ysql_yb_enable_mage"
	readCommitted = "yb_enable_read_committed_isolation"
	stackDBName   = "db"
	reapTimeout   = 30 * time.Minute
	stackDBMemory = 4.0
)

// creator makes universes for one provider.
type creator interface {
	Create(ctx context.Context, spec universe.Spec) (universe.Universe, error)
	// Reaps names what a killed process would have to delete for a universe of
	// this name.
	Reaps(name string) []reap.NSReap
}

// backing is the universe a stack was given, and how it goes away.
type backing struct {
	stackNS string
	key     string
	u       universe.Universe
}

type yugabytedCreator struct {
	prov *yugabytedk8s.Provider
	ctx  string
}

func (c yugabytedCreator) Create(ctx context.Context, spec universe.Spec) (universe.Universe, error) {
	u, err := c.prov.Create(ctx, spec)
	if u == nil {
		return nil, err
	}
	return u, err
}

func (c yugabytedCreator) Reaps(name string) []reap.NSReap {
	return []reap.NSReap{{Kind: reap.KindYugabytedK8s, KubeContext: c.ctx, Namespace: c.prov.Namespace(name)}}
}

// creatorFor builds the creator of a provider, scoped to tag so namespaces of
// different stacks and tests never meet.
func (p *Provider) creatorFor(ctx context.Context, prov universe.Provider, tag string) (creator, error) {
	if p.newCreator != nil {
		return p.newCreator(prov, tag)
	}
	switch prov {
	case universe.Yugabyted:
		c := p.cfg.core
		cfg := yugabytedk8s.Config{
			KubeContext:     p.cfg.Context,
			NamespacePrefix: c.GetString("db.yugabyted.namespace_prefix", yugabytedk8s.DefaultNamespacePrefix),
			Image:           p.cfg.Stack.DatabaseImage,
			PullUser:        p.cfg.Stack.QuayUser, PullToken: p.cfg.Stack.QuayToken,
			RunID: tag, Owner: p.cfg.Stack.Owner, TTL: p.cfg.Stack.TTL,
			ReadyTimeout: c.GetDuration("db.yugabyted.ready_timeout", yugabytedk8s.DefaultReadyTimeout),
			Logf:         p.logf,
		}
		return yugabytedCreator{prov: yugabytedk8s.New(cfg), ctx: p.cfg.Context}, nil
	case universe.YBA:
		return p.ybaCreator(ctx, tag)
	}
	return nil, fmt.Errorf("db.provider %q: want %s or %s (or %s for a stack's own database)", prov, universe.Yugabyted, universe.YBA, universe.Chart)
}

// dbProvider is the provider the run configuration names.
func (p *Provider) dbProvider() universe.Provider {
	if p.cfg.core == nil {
		return universe.Chart
	}
	return universe.Provider(p.cfg.core.GetString("db.provider", string(universe.Yugabyted)))
}

func (p *Provider) validateDatabase() error {
	switch p.dbProvider() {
	case universe.Chart:
		return nil
	case universe.Yugabyted:
		return nil
	case universe.YBA:
		return p.validateYBA()
	}
	return fmt.Errorf("db.provider = %q, want %s, %s or %s", p.dbProvider(), universe.Yugabyted, universe.YBA, universe.Chart)
}

// stackSpec is the spec of a stack's database: the test's own for a dedicated
// stack that declares one, else the run's db.* keys.
func (p *Provider) stackSpec(spec core.EnvSpec, shared bool) (universe.Spec, *core.Config) {
	c := p.cfg.core
	if c == nil {
		return universe.Spec{Provider: universe.Chart}, nil
	}
	if !shared && spec.BackingConfig != nil {
		if o := spec.BackingConfig(c); len(o) > 0 {
			c = c.Derive(o)
		}
	}
	var s universe.Spec
	if !shared && spec.Database != nil {
		s = *spec.Database
		if s.Provider == "" {
			s.Provider = p.dbProvider()
		}
	} else {
		s = universe.FromConfig(c, "db", stackDBName)
	}
	if s.Name == "" {
		s.Name = stackDBName
	}
	return s, c
}

// forMeko makes a spec a database meko can run on: its build, the mage flag and
// its preview allowance.
func forMeko(s universe.Spec, image string) universe.Spec {
	if s.Image == "" && s.Version == "" && s.Provider != universe.YBA {
		s.Image = image
	}
	s.MasterGflags = withMage(s.MasterGflags)
	s.TServerGflags = withMage(s.TServerGflags)
	if _, ok := s.MasterGflags["tablet_replicas_per_gib_limit"]; !ok {
		s.MasterGflags["tablet_replicas_per_gib_limit"] = "0"
	}
	// yugabyted turns read committed on and YBA leaves it off. Off, mage's
	// create_vlabel segfaulted in data migration 000001 and the stack never came up.
	// That call is gone from the migration, but cli_app still creates graphs through
	// mage and nothing has shown that safe with read committed off, so it stays on.
	if _, ok := s.TServerGflags[readCommitted]; !ok {
		s.TServerGflags[readCommitted] = "true"
	}
	return s
}

// withMage adds the mage flag, a preview flag that must be named in
// allowed_preview_flags_csv beside any flag already allowed.
func withMage(flags map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range flags {
		out[k] = v
	}
	out[magePreview] = "true"
	allowed := strings.TrimSpace(out["allowed_preview_flags_csv"])
	switch {
	case allowed == "":
		allowed = magePreview
	case !contains(strings.Split(allowed, ","), magePreview):
		allowed += "," + magePreview
	}
	out["allowed_preview_flags_csv"] = allowed
	return out
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if strings.TrimSpace(e) == s {
			return true
		}
	}
	return false
}

// prepareBacking stands up the stack's database. Its reap entry is recorded
// before the deploy starts, so a run killed halfway still names what to delete.
// A nil result is the chart's bundled database.
func (p *Provider) prepareBacking(ctx context.Context, spec core.EnvSpec, shared bool, tag, stackNS, image string) (*backing, *mekostack.Backing, error) {
	s, _ := p.stackSpec(spec, shared)
	if s.Provider == universe.Chart || s.Provider == "" {
		return nil, nil, nil
	}
	s = forMeko(s, image).WithDefaults()
	if err := s.Validate(); err != nil {
		return nil, nil, fmt.Errorf("the stack's database spec: %w", err)
	}
	cr, err := p.creatorFor(ctx, s.Provider, tag)
	if err != nil {
		return nil, nil, err
	}
	b := &backing{stackNS: stackNS, key: stackNS}
	p.trackBacking(b.key, cr.Reaps(s.Name))
	p.log("k8s: standing up the %s database for %s", s.Provider, stackNS)
	u, err := cr.Create(ctx, s)
	if u != nil {
		b.u = u
	}
	if err != nil {
		if terr := p.teardownBacking(b); terr != nil {
			p.log("k8s: removing the database of %s: %v", stackNS, terr)
		}
		return nil, nil, fmt.Errorf("the %s database: %w", s.Provider, err)
	}
	return b, stackBacking(ctx, u), nil
}

// stackBacking is the universe as the stack's pods reach it.
func stackBacking(ctx context.Context, u universe.Universe) *mekostack.Backing {
	host, port := u.ClusterAddr()
	c := u.Spec().Primary()
	place := map[string]int{}
	for z, n := range c.Placement {
		place[universe.ZoneName(z)] += n
	}
	// A one-zone spec names a placeholder, and YBA puts it in its provider's zone.
	if len(place) == 1 {
		if z := onlyZone(ctx, u); z != "" {
			place = map[string]int{z: c.Tservers()}
		}
	}
	return &mekostack.Backing{
		Mode: string(u.Provider()), Describe: fmt.Sprintf("%s database %s at %s:%d", u.Provider(), u.Name(), host, port),
		Host: host, Port: port, User: "yugabyte", SSLMode: "disable", Namespace: u.Namespace(),
		Replication: c.RF, Tservers: c.Tservers(), Placement: place,
	}
}

func onlyZone(ctx context.Context, u universe.Universe) string {
	nodes, err := u.Nodes(ctx)
	if err != nil {
		return ""
	}
	zone := ""
	for _, n := range nodes {
		if !n.Tserver || n.Cluster != universe.Primary {
			continue
		}
		if n.Zone == "" || (zone != "" && n.Zone != zone) {
			return ""
		}
		zone = n.Zone
	}
	return zone
}

// teardownBacking collects the database's logs, removes it, and takes its entry
// out of the plan once it is gone.
func (p *Provider) teardownBacking(b *backing) error {
	if b == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), reapTimeout)
	defer cancel()
	p.collectBacking(ctx, b)
	if b.u != nil {
		if err := b.u.Teardown(ctx); err != nil && !execk8s.IsNotFound(err) {
			return err
		}
	}
	p.untrackBacking(b.key)
	return nil
}

func (p *Provider) collectBacking(ctx context.Context, b *backing) {
	dir := p.cfg.Stack.DiagDir
	if dir == "" || b.u == nil || b.u.Namespace() == "" {
		return
	}
	ns := b.u.Namespace()
	kube := execk8s.New(execk8s.Config{Context: p.cfg.Context, Namespace: ns})
	diag.CollectUniverseK8sDiagnostics(ctx, kube, ns, mekostack.NewFileSink(dir+"/"+b.stackNS), p.logf)
}

func (p *Provider) trackBacking(key string, reaps []reap.NSReap) {
	p.mu.Lock()
	p.extra[key] = reaps
	fn := p.onPlanChange
	p.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (p *Provider) untrackBacking(key string) {
	p.mu.Lock()
	delete(p.extra, key)
	fn := p.onPlanChange
	p.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// Universes are the extra universes one test creates in its body. They belong to
// the test: Teardown removes them, and until then each is in the reap plan.
type Universes struct {
	p      *Provider
	tag    string
	prefix string

	mu   sync.Mutex
	made []*madeUniverse
}

type madeUniverse struct {
	u   universe.Universe
	key string
}

// Universes starts a set for one test. owner names the test (its ResourceSuffix),
// so namespaces of concurrent tests never collide.
func (e *Env) Universes(owner string) *Universes {
	return &Universes{p: e.provider, tag: e.provider.cfg.RunID + "-" + owner, prefix: owner}
}

// Create builds a universe, with the run's db.* values for whatever spec leaves
// out: the provider, the clusters, and each cluster's node shape.
func (s *Universes) Create(ctx context.Context, spec universe.Spec) (universe.Universe, error) {
	p := s.p
	if p.cfg.core == nil {
		return nil, errors.New("no run configuration, so no database provider")
	}
	if spec.Provider == "" {
		spec.Provider = p.dbProvider()
	}
	d := universe.FromConfig(p.cfg.core, "db", spec.Name)
	if len(spec.Clusters) == 0 {
		spec.Clusters = d.Clusters
	}
	spec = spec.WithNode(d.Primary().Node).WithDefaults()
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if spec.Provider == universe.Chart {
		return nil, errors.New("the chart database belongs to a stack, a universe cannot use it")
	}
	cr, err := p.creatorFor(ctx, spec.Provider, s.tag)
	if err != nil {
		return nil, err
	}
	key := "universe:" + s.tag + ":" + spec.Name
	p.trackBacking(key, cr.Reaps(spec.Name))
	p.log("k8s: creating the %s universe %s", spec.Provider, spec.Name)
	u, err := cr.Create(ctx, spec)
	m := &madeUniverse{u: u, key: key}
	if u != nil {
		s.mu.Lock()
		s.made = append(s.made, m)
		s.mu.Unlock()
	}
	if err != nil {
		if u == nil {
			p.untrackBacking(key)
		}
		return nil, fmt.Errorf("universe %s: %w", spec.Name, err)
	}
	return u, nil
}

// Teardown removes every universe the set made, last first.
func (s *Universes) Teardown(ctx context.Context) error {
	s.mu.Lock()
	made := s.made
	s.made = nil
	s.mu.Unlock()
	var errs []error
	for i := len(made) - 1; i >= 0; i-- {
		m := made[i]
		if err := m.u.Teardown(ctx); err != nil && !execk8s.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("%s: %w", m.u.Name(), err))
			continue
		}
		s.p.untrackBacking(m.key)
	}
	return errors.Join(errs...)
}
