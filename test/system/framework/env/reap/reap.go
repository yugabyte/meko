// Package reap is the cross-process teardown reconstructor for standing
// environments. `ampqa setup` leaves an environment up and records a serializable
// Plan in the registry; a LATER, SEPARATE `ampqa env rm` process — which shares
// no memory with the one that provisioned the env, so the in-process teardown
// closure is gone — decodes that Plan and reaps the real infrastructure it names.
//
// A Plan is plain, serializable data (the AGENTS.md "plain name→value" spirit):
// at most one cloud tag-reap (terminate every instance of a run tag, region-wide)
// plus any number of namespace reaps (delete a Kubernetes namespace + its PVCs /
// ClusterRoleBindings). No secrets are ever recorded — the cloud client
// re-resolves its region/credentials from the config + secret resolver `env rm`
// loads. Execute reuses the apps' OWN teardown code (ybk8s.Reap / ybastack.Reap /
// cloud.ReapByTag), so there is one teardown implementation per shape.
//
// Guaranteed no-leak (cardinal): Execute runs EVERY step even when one fails and
// returns a single aggregated, loud error naming what did not reap — a missed
// reap is a leak, never a silent success.
package reap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yugabyte/yb-amp/test/system/framework/apps/ampstack"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/cloud"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/ybastack"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/ybk8s"
	"github.com/yugabyte/yb-amp/test/system/framework/exec/k8s"
)

// Namespace-reap kinds. Each maps to an app's own teardown (ybk8s/ybastack) or
// the YBA-created-universe namespace discovery below.
const (
	KindYBK8s    = "ybk8s"    // a ybk8s deploy's namespace: ybk8s.Reap (managed-by guard, helm uninstall, PVCs, ns)
	KindYBAStack = "ybastack" // a ybastack YBA's namespace: ybastack.Reap (suffixed + chart CRBs, ns)
	KindAmp      = "amp"      // an ampstack control-plane namespace: ampstack.Reap (managed-by guard, ns delete)
	KindDiscover = "discover" // any YBA-created universe namespace named after Namespace (substring/prefix)
)

// Plan is the serializable set of reap actions that reclaim a standing env's real
// infrastructure. Exactly the shapes the preset suite produces (every preset also
// deploys the amp control plane into its OWN namespace, which StandUp appends as
// a KindAmp namespace reap — so no preset leaks its amp namespace):
//   - VM shapes (yugabyted_gce, raw_aws) → one Cloud tag-reap.
//   - yb_k8s → one KindYBK8s namespace.
//   - YBA (yba_k8s) → one KindYBAStack namespace (the control plane) plus, per the
//     universe backing, either a KindDiscover namespace (k8s universe pods) or a
//     Cloud tag-reap (a YBA-on-cloud universe's nodes).
type Plan struct {
	Cloud      *CloudReap `json:"cloud,omitempty"`
	Namespaces []NSReap   `json:"namespaces,omitempty"`
}

// CloudReap terminates every instance of RunTag via the cloud, region-wide (the
// airtight ownership-tag backstop). Region is informational — the cloud client
// resolves its region/credentials from the config `env rm` loads.
type CloudReap struct {
	Cloud  string `json:"cloud"` // "aws" | "gcp"
	RunTag string `json:"run_tag"`
	Region string `json:"region,omitempty"`
}

// NSReap deletes one Kubernetes namespace on KubeContext/Kubeconfig per Kind.
type NSReap struct {
	Kind        string   `json:"kind"`
	KubeContext string   `json:"kubecontext,omitempty"`
	Kubeconfig  string   `json:"kubeconfig,omitempty"`
	Namespace   string   `json:"namespace"`              // exact (ybk8s/ybastack); universe-name match (discover)
	CRBs        []string `json:"crbs,omitempty"`         // ybastack: the suffixed ClusterRoleBindings to delete
	HelmRelease string   `json:"helm_release,omitempty"` // ybastack: the REAL helm release (chart-scoped RBAC is named after it); "" → derive from namespace
}

// Empty reports whether the plan names nothing to reap.
func (p *Plan) Empty() bool {
	return p == nil || (p.Cloud == nil && len(p.Namespaces) == 0)
}

// JSON serializes the plan for the registry record. An empty plan yields "".
func (p *Plan) JSON() (string, error) {
	if p.Empty() {
		return "", nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("marshal reap plan: %w", err)
	}
	return string(b), nil
}

// FromJSON decodes a plan a setup process recorded. "" yields a nil plan.
func FromJSON(s string) (*Plan, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var p Plan
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return nil, fmt.Errorf("decode reap plan: %w", err)
	}
	return &p, nil
}

// Execute reaps everything the plan names, in a process separate from the one
// that provisioned it. cfg + resolve let the cloud client re-resolve its
// credentials here (no secret is ever carried in the plan) while the plan's
// recorded region pins WHERE it reaps. It runs every step even on partial
// failure and returns one aggregated error naming what did not reap.
//
// force controls the zero-confirmation guard: a cloud reap that describes ZERO
// instances cannot confirm it acted on the expected resources (the region or
// credentials may be defaulted/divergent, so it may have looked in the wrong
// place). With force=false that is an error and the caller keeps the record;
// with force=true it is tolerated and the record may be dropped.
func Execute(ctx context.Context, p *Plan, cfg cloud.ConfigGetter, resolve cloud.SecretResolver, force bool) error {
	return newExecutor(cfg, resolve).run(ctx, p, force)
}

// executor holds the reap seams so unit tests substitute fakes for the real
// cloud/kubectl calls.
type executor struct {
	cloudReap func(ctx context.Context, c *CloudReap, force bool) error
	nsReap    func(ctx context.Context, n NSReap) error
}

// confirmedReaper is the optional richer reap the cloud adapters expose: it
// reports how many instances it acted on, so a zero-count reap in a possibly
// divergent region is not mistaken for a successful reclaim. A driver that does
// not implement it falls back to the plain, unconfirmed ReapByTag.
type confirmedReaper interface {
	ReapByTagConfirmed(ctx context.Context, runTag string) (int, error)
}

// newCloudVM builds the cloud client the default reap drives; a package var so
// tests substitute a fake without a real cloud.
var newCloudVM = cloud.New

func newExecutor(cfg cloud.ConfigGetter, resolve cloud.SecretResolver) *executor {
	return &executor{
		cloudReap: func(ctx context.Context, c *CloudReap, force bool) error {
			// Reap in the region the infra was CREATED in (recorded in the plan),
			// not the ambient config default — reaping the wrong region silently
			// leaks the real instances while "succeeding" against an empty region.
			getter := cfg
			if c.Region != "" {
				getter = regionOverride{ConfigGetter: cfg, cloud: c.Cloud, region: c.Region}
			}
			vm, err := newCloudVM(c.Cloud, getter, resolve)
			if err != nil {
				return fmt.Errorf("build %s client: %w", c.Cloud, err)
			}
			confirmer, ok := vm.(confirmedReaper)
			if !ok {
				return vm.ReapByTag(ctx, c.RunTag)
			}
			n, err := confirmer.ReapByTagConfirmed(ctx, c.RunTag)
			if err != nil {
				return err
			}
			if n == 0 && !force {
				return fmt.Errorf("described ZERO instances in region %q — refusing to treat as reaped (wrong region/credentials?); pass --force to drop the record anyway", effectiveRegion(c.Region))
			}
			return nil
		},
		nsReap: defaultNSReap,
	}
}

func effectiveRegion(r string) string {
	if r == "" {
		return "config default"
	}
	return r
}

func (e *executor) run(ctx context.Context, p *Plan, force bool) error {
	if p.Empty() {
		return nil
	}
	var errs []string
	if p.Cloud != nil {
		if err := e.cloudReap(ctx, p.Cloud, force); err != nil {
			errs = append(errs, fmt.Sprintf("cloud reap (%s tag=%s): %v", p.Cloud.Cloud, p.Cloud.RunTag, err))
		}
	}
	for _, n := range p.Namespaces {
		if err := e.nsReap(ctx, n); err != nil {
			errs = append(errs, fmt.Sprintf("namespace reap (%s %s): %v", n.Kind, n.Namespace, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("reap incomplete — infrastructure may be leaked: %s", strings.Join(errs, "; "))
	}
	return nil
}

// regionOverride wraps a cloud.ConfigGetter so the target cloud's region key
// (apps.cloud.<cloud>.region) resolves to the plan's recorded region — the
// region the infrastructure was created in — instead of the ambient config
// default. Every other key delegates unchanged.
type regionOverride struct {
	cloud.ConfigGetter
	cloud  string
	region string
}

func (r regionOverride) GetString(key, def string) string {
	if r.region != "" && key == "apps.cloud."+r.cloud+".region" {
		return r.region
	}
	return r.ConfigGetter.GetString(key, def)
}

func defaultNSReap(ctx context.Context, n NSReap) error {
	switch n.Kind {
	case KindYBK8s:
		return ybk8s.Reap(ctx, n.KubeContext, n.Kubeconfig, n.Namespace)
	case KindYBAStack:
		return ybastack.Reap(ctx, n.KubeContext, n.Kubeconfig, n.Namespace, n.HelmRelease, n.CRBs)
	case KindAmp:
		return ampstack.Reap(ctx, n.KubeContext, n.Kubeconfig, n.Namespace)
	case KindDiscover:
		return discoverAndDelete(ctx, n.KubeContext, n.Kubeconfig, n.Namespace)
	default:
		return fmt.Errorf("unknown namespace reap kind %q", n.Kind)
	}
}

// discoverAndDelete deletes every namespace on the target cluster named after a
// YBA-created universe (the exact name or a "<name>-" prefix), the analogue of
// env/universe's namespace backstop for a universe standing up ON k8s: YBA names
// the universe namespace after the universe, and the universe name is the unique
// ampqa-<runTag>, so this match is safe.
func discoverAndDelete(ctx context.Context, kubeContext, kubeconfig, universeName string) error {
	if universeName == "" {
		return fmt.Errorf("discover reap: empty universe name")
	}
	kube := k8s.New(k8s.Config{Context: kubeContext}).WithRunner(k8s.KubeconfigRunner(kubeconfig))
	out, err := kube.GetSelector(ctx, "namespaces", "", "{.items[*].metadata.name}")
	if err != nil {
		return fmt.Errorf("list namespaces: %w", err)
	}
	var bad []string
	for _, ns := range strings.Fields(out) {
		if ns == universeName || strings.HasPrefix(ns, universeName+"-") {
			if derr := kube.DeleteNamespace(ctx, ns); derr != nil {
				bad = append(bad, fmt.Sprintf("delete namespace %s: %v", ns, derr))
			}
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s", strings.Join(bad, "; "))
	}
	return nil
}
