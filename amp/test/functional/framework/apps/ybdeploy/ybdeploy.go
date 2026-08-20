// Package ybdeploy selects and provisions the YugabyteDB an amp control plane
// sits on, so any functional/perf test can run against a different deployment
// WITHOUT knowing which one. A run-level knob (env.yb_deploy) chooses the
// deployment; a test may declare a preference (core.EnvSpec.PreferredYBDeploy);
// the knob always wins over the preference.
//
// It is the glue that turns a chosen mode into a ybbacking.BackingStore (which
// the provider maps into an ampstack external backing via
// ybbacking.ExternalBacking) plus a teardown that reaps ONLY what this call
// created. The concrete cluster bring-up lives in the apps it composes
// (apps/ybk8s, apps/yba) or the env it reuses (env/universe); this package owns
// no lifecycle of its own beyond dispatch. builtin_yugabyted provisions nothing:
// the caller keeps ampstack's own built-in yugabyted.
package ybdeploy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/yba"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybbacking"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybk8s"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybpod"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/reap"
	"github.com/yugabyte/amp/test/functional/framework/env/universe"
)

// Deploy modes — the accepted env.yb_deploy knob values.
const (
	ModeBuiltin        = "builtin_yugabyted" // ampstack's own single-node yugabyted (no external backing)
	ModeExternal       = "external"          // a cluster owned elsewhere, from env.ampstack.backing.*
	ModeYBK8s          = "yb_k8s"            // a YugabyteDB deployed into k8s (apps/ybk8s), co-located
	ModeYBPod          = "yb_pod"            // a yugabyted-in-pod from the release tarball (apps/ybpod), co-located
	ModeYBAUniverse    = "yba_universe"      // an attached-or-created YBA universe (apps/yba)
	ModeYBAProvisioned = "yba_provisioned"   // an owned YBA + universe we stand up (env/universe)
)

// ValidModes lists every accepted env.yb_deploy value, in the unknown-value error.
var ValidModes = []string{ModeBuiltin, ModeExternal, ModeYBK8s, ModeYBPod, ModeYBAUniverse, ModeYBAProvisioned}

// Resolve picks the deploy mode. Precedence: the env.yb_deploy knob (when set to
// a non-empty value) beats spec.PreferredYBDeploy, which beats the built-in
// default. An empty knob honours the preference — so the knob is an override,
// not a floor.
func Resolve(cfg *core.Config, spec core.EnvSpec) string {
	if cfg.Has("env.yb_deploy") {
		if v := cfg.GetString("env.yb_deploy", ""); v != "" {
			return v
		}
	}
	if spec.PreferredYBDeploy != "" {
		return spec.PreferredYBDeploy
	}
	return ModeBuiltin
}

// Requested reports whether the deploy mode was asked for EXPLICITLY, via the
// env.yb_deploy knob, rather than merely defaulted from a test's preference.
// It is what separates "this run wants YBA" from "YBA is what runs get unless
// something is missing", and so decides whether a failed backing may be
// silently downgraded.
func Requested(cfg *core.Config) bool {
	return cfg != nil && cfg.Has("env.yb_deploy") && cfg.GetString("env.yb_deploy", "") != ""
}

// Provision stands up the deployment named by mode (the value Resolve produced)
// and returns it as a BackingStore plus a teardown reaping ONLY what this call
// created. builtin_yugabyted returns (nil, nil, nil): the caller keeps ampstack's
// built-in yugabyted. An unknown mode is an error listing the valid values.
//
// ctx bounds provisioning WORK only. Anything that must outlive this call (a
// deployment's port-forwards etc.) is owned by the composed app and rooted in its
// own background scope, released solely by the returned teardown (AGENTS.md §5).
// The teardown, when non-nil, runs on the caller's own teardown deadline.
func Provision(ctx context.Context, cfg *core.Config, mode, runTag string, logf func(string, ...any)) (ybbacking.BackingStore, func(context.Context) error, error) {
	store, td, _, err := ProvisionWithPlan(ctx, cfg, mode, runTag, logf)
	return store, td, err
}

// ProvisionWithPlan is Provision plus the serializable reap.Plan naming what it
// created, so a LATER process can reclaim it when this one is killed before its
// teardown runs. An empty plan means the mode provisioned nothing reapable.
func ProvisionWithPlan(ctx context.Context, cfg *core.Config, mode, runTag string, logf func(string, ...any)) (ybbacking.BackingStore, func(context.Context) error, *reap.Plan, error) {
	switch mode {
	case "", ModeBuiltin:
		return nil, nil, nil, nil
	case ModeExternal:
		store, td, err := provisionExternal(cfg)
		return store, td, nil, err
	case ModeYBK8s:
		store, td, err := provisionYBK8s(ctx, cfg, runTag, logf)
		return store, td, ybk8sPlan(cfg, store), err
	case ModeYBPod:
		store, td, err := provisionYBPod(ctx, cfg, runTag, logf)
		return store, td, nil, err
	case ModeYBAUniverse:
		store, td, err := provisionYBAUniverse(ctx, cfg, logf)
		return store, td, nil, err
	case ModeYBAProvisioned:
		return provisionYBAProvisionedWithPlan(ctx, cfg, runTag, logf)
	default:
		return nil, nil, nil, fmt.Errorf("unknown env.yb_deploy %q: valid values are %s", mode, strings.Join(ValidModes, ", "))
	}
}

// ybk8sPlan names the co-located deploy's namespace so a killed run's YB pods
// are reclaimed.
func ybk8sPlan(cfg *core.Config, store ybbacking.BackingStore) *reap.Plan {
	ns := ""
	if kc, ok := store.(interface{ KubeColocation() (string, string) }); ok {
		ns, _ = kc.KubeColocation()
	}
	if ns == "" {
		return nil
	}
	k := ybk8s.FromConfig(cfg)
	return &reap.Plan{Namespaces: []reap.NSReap{{
		Kind: reap.KindYBK8s, KubeContext: k.KubeContext, Kubeconfig: k.Kubeconfig, Namespace: ns,
	}}}
}

// provisionExternal points the control plane at a cluster owned elsewhere, read
// from the existing env.ampstack.backing.* knobs (no new keys). It provisions
// nothing and has no teardown — the external cluster is never touched.
func provisionExternal(cfg *core.Config) (ybbacking.BackingStore, func(context.Context) error, error) {
	b := ampstack.FromConfig(cfg).Backing
	if b.HostPorts == "" {
		return nil, nil, fmt.Errorf("env.yb_deploy=external requires env.ampstack.backing.host_ports (CSV of tserver host:port)")
	}
	addrs := strings.Split(b.HostPorts, ",")
	host, port := ybk8s.FirstHostPort(addrs[0])
	return ybbacking.Static{
		Conn:  pg.ConnInfo{Host: host, Port: port, User: b.User, Password: b.Password, SSLMode: b.SSLMode},
		Addrs: addrs,
		Desc:  fmt.Sprintf("external YugabyteDB (%d host:port, primary %s)", len(addrs), addrs[0]),
	}, nil, nil
}

// provisionYBK8s deploys a YugabyteDB into k8s (apps/ybk8s) and adapts it as a
// co-located backing (in-cluster DNS for amp, tailnet pod IPs for the admin
// create). Teardown deletes the deploy's namespace + PVCs. Mirrors the app calls
// in suites/features/provisioners_helper.go's provisionYBK8s (no reap plan, no T).
func provisionYBK8s(ctx context.Context, cfg *core.Config, runTag string, logf func(string, ...any)) (ybbacking.BackingStore, func(context.Context) error, error) {
	secrets, err := core.NewSecrets(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("secrets: %w", err)
	}
	d := ybk8s.New(ybk8s.FromConfig(cfg), runTag).WithLogf(logf).WithSecretResolver(secrets.Value)
	info, err := d.Deploy(ctx)
	if err != nil {
		_ = d.Teardown(context.Background()) // reap the partial deploy
		return nil, nil, fmt.Errorf("provision yb in k8s: %w", err)
	}
	host, port := ybk8s.FirstHostPort(info.TailnetAddrs[0])
	conn := pg.ConnInfo{
		Host:     host,
		Port:     port,
		User:     info.User,
		Database: info.User, // the always-present maintenance DB matches the superuser name
		SSLMode:  "disable",
	}
	teardown := func(ctx context.Context) error { return d.Teardown(ctx) }
	// The endpoint's thin client dials the tserver RPC port (9100) on the SAME
	// host it gets for YSQL. The tserver Service does not carry that port —
	// kube-proxy blackholes ClusterIP:9100 and the endpoint crash-loops on a
	// heartbeat timeout — so the controller must get the per-pod stable DNS
	// names, where every port answers, never the Service.
	return ybbacking.FromYBK8s(conn, info.TailnetAddrs, info.TserverHostPorts), teardown, nil
}

// provisionYBPod stands up a yugabyted-in-pod from the release TARBALL (apps/ybpod)
// and adapts it as a co-located backing — the image-promotion-independent path for
// a build whose container image was never published (only its S3 tarball exists).
// Teardown deletes the deploy's namespace. Mirrors provisionYBK8s; the secret
// resolver serves an optional authenticated tarball_secret.
func provisionYBPod(ctx context.Context, cfg *core.Config, runTag string, logf func(string, ...any)) (ybbacking.BackingStore, func(context.Context) error, error) {
	secrets, err := core.NewSecrets(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("secrets: %w", err)
	}
	d := ybpod.New(ybpod.FromConfig(cfg), runTag).WithLogf(logf).WithSecretResolver(secrets.Value)
	info, err := d.Deploy(ctx)
	if err != nil {
		_ = d.Teardown(context.Background()) // reap the partial deploy
		return nil, nil, fmt.Errorf("provision yugabyted-in-pod: %w", err)
	}
	teardown := func(ctx context.Context) error { return d.Teardown(ctx) }
	return info.Colocated(), teardown, nil
}

// provisionYBAUniverse attaches an existing YBA universe (env.universe.yba.universe)
// or creates one from functional.yba.create_spec. Teardown deletes ONLY a
// universe this call CREATED — an attached (someone else's) universe is never
// touched (AGENTS.md §8 hygiene). Reuses apps/yba's client, not the suite types.
func provisionYBAUniverse(ctx context.Context, cfg *core.Config, logf func(string, ...any)) (ybbacking.BackingStore, func(context.Context) error, error) {
	url := cfg.GetString("env.universe.yba.url", "")
	customer := cfg.GetString("env.universe.yba.customer_uuid", "")
	token := cfg.GetString("env.universe.yba.api_token", "")
	if url == "" || customer == "" || token == "" {
		return nil, nil, fmt.Errorf("env.yb_deploy=yba_universe requires env.universe.yba.url, customer_uuid and api_token")
	}
	cl := yba.New(url, customer, token, yba.WithHTTPClient(ybaHTTPClient(cfg.GetBool("env.universe.yba.insecure_tls", true))))

	var u yba.Universe
	var createdID string
	if ref := cfg.GetString("env.universe.yba.universe", ""); ref != "" {
		got, err := cl.GetUniverse(ctx, ref)
		if err != nil {
			return nil, nil, fmt.Errorf("attach universe %q: %w", ref, err)
		}
		u = got
	} else {
		spec := cfg.GetMap("functional.yba.create_spec", nil)
		if len(spec) == 0 {
			return nil, nil, fmt.Errorf("env.yb_deploy=yba_universe: set env.universe.yba.universe to attach, or functional.yba.create_spec to create")
		}
		ref, err := cl.CreateUniverseAndWait(ctx, spec)
		if err != nil {
			return nil, nil, fmt.Errorf("create universe: %w", err)
		}
		got, err := cl.GetUniverse(ctx, ref.ResourceUUID)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve created universe %s: %w", ref.ResourceUUID, err)
		}
		u = got
		createdID = u.UniverseUUID
	}

	var teardown func(context.Context) error
	if createdID != "" {
		teardown = func(ctx context.Context) error {
			_, err := cl.DeleteUniverseAndWait(ctx, createdID)
			return err
		}
	}
	bs, err := ybbacking.FromYBAUniverse(u)
	if err != nil {
		return nil, teardown, err
	}
	return bs, teardown, nil
}

// provisionYBAProvisioned stands up an OWNED YBA + universe via env/universe's
// provision_yba mode and adapts its endpoint into a backing store. The whole
// deploy→provider→universe flow and its partial-safe teardown live in the env;
// teardown here delegates to the env's own Teardown. Mirrors the app calls in
// provisioners_helper.go's provisionYBAProvisioned (no reap plan, no T). The
// per-runTag seed keeps naming deterministic.
func provisionYBAProvisioned(ctx context.Context, cfg *core.Config, runTag string, logf func(string, ...any)) (ybbacking.BackingStore, func(context.Context) error, error) {
	store, td, _, err := provisionYBAProvisionedWithPlan(ctx, cfg, runTag, logf)
	return store, td, err
}

func provisionYBAProvisionedWithPlan(ctx context.Context, cfg *core.Config, runTag string, logf func(string, ...any)) (ybbacking.BackingStore, func(context.Context) error, *reap.Plan, error) {
	secrets, err := core.NewSecrets(cfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("secrets: %w", err)
	}
	ucfg := universe.FromConfig(cfg)
	ucfg.Mode = universe.ModeProvisionYBA
	prov := universe.New(ucfg,
		universe.WithLogf(logf),
		universe.WithProvisionYBA(cfg, secrets.Value))

	seed := core.NewRunSeed(0, true).Fork("yb_deploy/" + runTag)
	env, err := prov.Provision(ctx, core.EnvSpec{Sharing: core.Exclusive, Shape: core.Universe}, seed)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("provision owned yba universe: %w", err)
	}
	teardown := func(ctx context.Context) error { return prov.Teardown(ctx, env) }

	acc, ok := env.(interface {
		PGEndpoint() pg.ConnInfo
		NodeAddrs() []string
	})
	if !ok {
		return nil, teardown, nil, fmt.Errorf("provisioned yba env does not expose a YSQL endpoint (%T)", env)
	}
	// A kubernetes-provider universe exposes its tserver in-cluster addresses
	// separately from the operator-reachable pod IPs (env/universe.Env). When
	// present, they drive the co-located split; a VM universe exposes none.
	var inCluster []string
	if k, ok := env.(interface{ InClusterAddrs() []string }); ok {
		inCluster = k.InClusterAddrs()
	}
	// The env is carried along, not dropped: it is the only handle to the YBA
	// that manages this universe, and a maintenance test needs to drive it.
	backing := UniverseBacking{
		BackingStore: ybaProvisionedBacking(acc.PGEndpoint(), acc.NodeAddrs(), inCluster),
		env:          env,
	}
	return backing, teardown, ybaReapPlan(env), nil
}

// ybaReapPlan names the YBA control plane and, for a kubernetes universe, the
// namespace YBA created for its nodes; a VM universe reaps by ownership tag
// instead. The handles come from the env that provisioned them, so nothing is
// derived from a naming convention.
func ybaReapPlan(env core.Environment) *reap.Plan {
	h, ok := env.(interface {
		ProvisionedHandles() universe.ProvisionedHandles
	})
	if !ok {
		return nil
	}
	ph := h.ProvisionedHandles()
	p := &reap.Plan{}
	if ph.YBANamespace != "" {
		p.Namespaces = append(p.Namespaces, reap.NSReap{
			Kind: reap.KindYBAStack, KubeContext: ph.KubeContext, Kubeconfig: ph.Kubeconfig,
			Namespace: ph.YBANamespace, CRBs: ph.CRBs, HelmRelease: ph.HelmRelease,
		})
	}
	if ph.UniverseName != "" {
		p.Namespaces = append(p.Namespaces, reap.NSReap{
			Kind: reap.KindDiscover, KubeContext: ph.KubeContext, Kubeconfig: ph.Kubeconfig,
			Namespace: ph.UniverseName,
		})
	}
	if ph.RunTag != "" && ph.ProviderType != "" && ph.ProviderType != "kubernetes" {
		p.Cloud = &reap.CloudReap{Cloud: ph.ProviderType, RunTag: ph.RunTag, Region: ph.Region}
	}
	if p.Empty() {
		return nil
	}
	return p
}

// ybaProvisionedBacking adapts a provisioned-universe endpoint into a backing
// store. The kube location a kubernetes universe carries is what keeps the
// yb_storage CREATE and the storage-side test assertions on kubectl: dialling
// the universe from here needs a route into the pod CIDR, and on a cluster
// where that range is shadowed by another advertised tailnet route every
// attempt times out against a perfectly healthy RF=3 universe.
func ybaProvisionedBacking(conn pg.ConnInfo, addrs, inClusterAddrs []string) ybbacking.BackingStore {
	return ybbacking.FromProvisionedUniverse(conn, addrs, inClusterAddrs)
}

// UniverseBacking is a backing store that also carries the universe env it came
// from, so a test can MAINTAIN the universe (upgrade it, change its gflags) and
// not merely connect to it. Only the yba_provisioned mode produces one; every
// other backing is just a place to write.
//
// It forwards the OPTIONAL surfaces a BackingStore may implement
// (ControllerHostPortsCSV, KubeColocation) explicitly. Embedding the interface
// alone promotes only the methods BackingStore declares, so the type assertions
// ybbacking.ExternalBacking makes for those two would fail against the wrapper
// and silently lose the co-located path — the amp storage-DB create would go
// back to dialling pod IPs the coordinator has no route to.
type UniverseBacking struct {
	ybbacking.BackingStore
	env core.Environment
}

// ControllerHostPortsCSV forwards the in-cluster address split when the wrapped
// store provides one.
func (b UniverseBacking) ControllerHostPortsCSV() string {
	if c, ok := b.BackingStore.(interface{ ControllerHostPortsCSV() string }); ok {
		return c.ControllerHostPortsCSV()
	}
	return b.BackingStore.HostPortsCSV()
}

// KubeColocation forwards where the wrapped store's tservers live.
func (b UniverseBacking) KubeColocation() (namespace, pod string) {
	if k, ok := b.BackingStore.(interface{ KubeColocation() (string, string) }); ok {
		return k.KubeColocation()
	}
	return "", ""
}

// UniverseEnv is the env that provisioned the universe under this backing.
func (b UniverseBacking) UniverseEnv() core.Environment { return b.env }

// UniverseEnvOf returns the universe env behind a backing store, if it has one.
func UniverseEnvOf(store ybbacking.BackingStore) core.Environment {
	if u, ok := store.(interface{ UniverseEnv() core.Environment }); ok {
		return u.UniverseEnv()
	}
	return nil
}

// ybaHTTPClient builds the YBA HTTP client, skipping TLS verification when the
// server is self-signed (the common dev case).
func ybaHTTPClient(insecure bool) *http.Client {
	hc := &http.Client{Timeout: 60 * time.Second}
	if insecure {
		hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // YBA is commonly self-signed; opt-in via config
	}
	return hc
}
