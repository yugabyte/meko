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
	switch mode {
	case "", ModeBuiltin:
		return nil, nil, nil
	case ModeExternal:
		return provisionExternal(cfg)
	case ModeYBK8s:
		return provisionYBK8s(ctx, cfg, runTag, logf)
	case ModeYBPod:
		return provisionYBPod(ctx, cfg, runTag, logf)
	case ModeYBAUniverse:
		return provisionYBAUniverse(ctx, cfg, logf)
	case ModeYBAProvisioned:
		return provisionYBAProvisioned(ctx, cfg, runTag, logf)
	default:
		return nil, nil, fmt.Errorf("unknown env.yb_deploy %q: valid values are %s", mode, strings.Join(ValidModes, ", "))
	}
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
	return ybbacking.FromYBK8s(conn, info.TailnetAddrs, []string{info.InClusterHostPorts}), teardown, nil
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
	secrets, err := core.NewSecrets(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("secrets: %w", err)
	}
	ucfg := universe.FromConfig(cfg)
	ucfg.Mode = universe.ModeProvisionYBA
	prov := universe.New(ucfg,
		universe.WithLogf(logf),
		universe.WithProvisionYBA(cfg, secrets.Value))

	seed := core.NewRunSeed(0, true).Fork("yb_deploy/" + runTag)
	env, err := prov.Provision(ctx, core.EnvSpec{Sharing: core.Exclusive, Shape: core.Universe}, seed)
	if err != nil {
		return nil, nil, fmt.Errorf("provision owned yba universe: %w", err)
	}
	teardown := func(ctx context.Context) error { return prov.Teardown(ctx, env) }

	acc, ok := env.(interface {
		PGEndpoint() pg.ConnInfo
		NodeAddrs() []string
	})
	if !ok {
		return nil, teardown, fmt.Errorf("provisioned yba env does not expose a YSQL endpoint (%T)", env)
	}
	// A kubernetes-provider universe exposes its tserver in-cluster addresses
	// separately from the operator-reachable pod IPs (env/universe.Env). When
	// present, they drive the co-located split; a VM universe exposes none.
	var inCluster []string
	if k, ok := env.(interface{ InClusterAddrs() []string }); ok {
		inCluster = k.InClusterAddrs()
	}
	return ybaProvisionedBacking(acc.PGEndpoint(), acc.NodeAddrs(), inCluster), teardown, nil
}

// ybaProvisionedBacking adapts a provisioned-universe endpoint into a backing
// store. A kubernetes universe (non-empty inClusterAddrs) yields a co-located
// ybbacking.Colocated: the amp controller + endpoints dial the tserver
// IN-CLUSTER addresses, while the one-shot yb_storage CREATE (run from the
// operator/coordinator) uses the tailnet pod IPs in conn/addrs — the same split
// the yb_k8s deploy mode draws, because a plain Static would hand the in-cluster
// controller the operator-oriented pod IPs. A VM universe (empty inClusterAddrs
// — its node IPs are reachable both ways) stays a plain Static.
func ybaProvisionedBacking(conn pg.ConnInfo, addrs, inClusterAddrs []string) ybbacking.BackingStore {
	if len(inClusterAddrs) > 0 {
		return ybbacking.Colocated{
			Static: ybbacking.Static{
				Conn:  conn,
				Addrs: addrs,
				Desc:  fmt.Sprintf("provisioned YBA k8s universe (%d tservers, amp via %s, coordinator via %s)", len(inClusterAddrs), strings.Join(inClusterAddrs, ","), conn.Addr()),
			},
			ControllerAddrs: inClusterAddrs,
		}
	}
	return ybbacking.Static{
		Conn:  conn,
		Addrs: addrs,
		Desc:  fmt.Sprintf("provisioned YBA universe (%d live tservers, primary %s)", len(addrs), conn.Addr()),
	}
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
