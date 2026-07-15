package features

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"path"
	"time"

	"github.com/yugabyte/yb-amp/test/system/framework/apps/cloud"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/pg"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/yba"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/ybbacking"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/ybk8s"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/yugabyte"
	"github.com/yugabyte/yb-amp/test/system/framework/apps/yugabyted"
	"github.com/yugabyte/yb-amp/test/system/framework/artifact"
	"github.com/yugabyte/yb-amp/test/system/framework/core"
	"github.com/yugabyte/yb-amp/test/system/framework/env/reap"
	"github.com/yugabyte/yb-amp/test/system/framework/env/universe"
	"github.com/yugabyte/yb-amp/test/system/framework/env/vm"
	"github.com/yugabyte/yb-amp/test/system/framework/exec"
)

// vmHost is the cloud-agnostic VM environment surface the provisioners consume:
// the per-node executors and the private/public addresses. *vm.Env satisfies it.
type vmHost interface {
	Nodes() []exec.RemoteExecutor
	NodeAddrs() []string
	PublicAddrs() []string
}

// advertiseAddrs picks the address each node advertises to peers (and the
// coordinator reaches YSQL at): the private IPs (in-VPC coordinator, the
// default) or the public IPs.
func (cfg Config) advertiseAddrs(host vmHost) []string {
	if cfg.Advertise == "public" {
		return host.PublicAddrs()
	}
	return host.NodeAddrs()
}

func (cfg Config) clusterRF(nodes int) int {
	if cfg.ClusterRF > 0 {
		return cfg.ClusterRF
	}
	return nodes
}

func (cfg Config) ysqlPort() int {
	if cfg.ClusterYSQLPort > 0 {
		return cfg.ClusterYSQLPort
	}
	return DefaultClusterYSQLPort
}

// provisionVMHost launches the VM instances (reusing the cloud-agnostic vm
// provider — no duplicated launch/SSH/reap logic; the cloud is env.vm.cloud,
// default aws), registers their termination on the teardown stack, and returns
// the live host plus its advertise addresses. It resolves the concrete cloud
// driver via cloud.New, which reads apps.cloud.<cloud>.* and resolves any
// optional explicit credentials through the secrets resolver.
func (cfg Config) provisionVMHost(ctx context.Context, t *core.T, td *teardownStack) (vmHost, []string, *reap.Plan, error) {
	vmCfg := cfg.vmConfig(t.Options)
	secrets, err := core.NewSecrets(t.Options)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("secrets: %w", err)
	}
	client, err := cloud.New(vmCfg.Cloud, t.Options, secrets.Value)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build %s vm client: %w", vmCfg.Cloud, err)
	}
	prov := vm.New(vmCfg, client, vm.WithLogf(t.Masker().Logf(log.Printf)))
	env, err := prov.Provision(ctx, core.EnvSpec{Sharing: core.Exclusive}, t.Seed.Fork("host"))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("provision vm host: %w", err)
	}
	// Registered immediately: any later failure still terminates the instances.
	td.push("terminate vm host", func(ctx context.Context) error { return prov.Teardown(ctx, env) })

	host, ok := env.(vmHost)
	if !ok {
		return nil, nil, nil, fmt.Errorf("vm env does not expose the host node surface (%T)", env)
	}
	addrs := cfg.advertiseAddrs(host)
	if len(addrs) == 0 {
		return nil, nil, nil, fmt.Errorf("aws host has no advertise addresses")
	}
	// Cloud tag-reap plan: the REAL run tag the vm provider stamped on the
	// instances (env.RunTag(), not a guessed seed suffix), so a separate-process
	// `env rm` terminates exactly this host's instances region-wide.
	plan := cloudReapPlan(vmCfg.Cloud, env, client.Meta().Region)
	return host, addrs, plan, nil
}

// cloudReapPlan builds the ownership-tag reap plan for a vm-backed host from the
// live env's real run tag (the tag the cloud driver stamped), or nil when the env
// does not expose one (nothing to reap by tag — teardown then relies on the
// in-process handle only, which a standing env never has, so this must resolve).
func cloudReapPlan(cloud string, env core.Environment, region string) *reap.Plan {
	tagged, ok := env.(interface{ RunTag() string })
	if !ok || tagged.RunTag() == "" {
		return nil
	}
	return &reap.Plan{Cloud: &reap.CloudReap{Cloud: cloud, RunTag: tagged.RunTag(), Region: region}}
}

// clusterDeploy resolves the yugabytedb artifact + its install dir for the
// yugabyted/raw-yugabyte apps' own Spec.Deploy seam, when the awsvm AMI ships
// no pre-installed release: ClusterInstallDir empty AND ClusterDeploy true.
// ok=false means the caller leaves InstallDir/Deploy exactly as configured
// (the pre-baked-AMI path) — not an error.
//
// installDir is ClusterDeployDir joined with the resolved spec.Version, which
// the yugabytedb artifact declares explicitly as "yugabyte-{yugabytedb}"
// (config: artifacts.yugabytedb.version) — the real release tarball's
// top-level directory on extract, which drops the build number and os/arch
// suffix the archive filename carries.
func (cfg Config) clusterDeploy(c *core.Config) (spec artifact.Spec, installDir string, ok bool, err error) {
	if cfg.ClusterInstallDir != "" || !cfg.ClusterDeploy {
		return artifact.Spec{}, "", false, nil
	}
	specs, err := artifact.Resolve(c)
	if err != nil {
		return artifact.Spec{}, "", false, fmt.Errorf("resolve artifacts: %w", err)
	}
	spec, ok = specs["yugabytedb"]
	if !ok {
		return artifact.Spec{}, "", false, fmt.Errorf("functional.cluster.deploy is true but artifacts.yugabytedb is not declared")
	}
	return spec, path.Join(cfg.ClusterDeployDir, spec.Version), true, nil
}

// provisionYugabytedOnAWS: vm host → a yugabyted cluster over its nodes.
func (cfg Config) provisionYugabytedOnAWS(ctx context.Context, t *core.T, td *teardownStack, logf func(string, ...any)) (ybbacking.BackingStore, *reap.Plan, error) {
	host, addrs, plan, err := cfg.provisionVMHost(ctx, t, td)
	if err != nil {
		return nil, nil, err
	}
	execs := host.Nodes()
	nodes := make([]yugabyted.Node, len(execs))
	for i := range execs {
		nodes[i] = yugabyted.Node{Exec: execs[i], Advertise: addrs[i]}
	}
	spec := yugabyted.Spec{
		InstallDir: cfg.ClusterInstallDir,
		BaseDir:    cfg.ClusterBaseDir,
		DataDir:    cfg.ClusterDataDir,
		RF:         cfg.clusterRF(len(nodes)),
		YSQLPort:   cfg.ysqlPort(),
	}
	artSpec, installDir, deployNow, err := cfg.clusterDeploy(t.Options)
	if err != nil {
		return nil, nil, err
	}
	if deployNow {
		spec.Deploy = &yugabyted.DeploySpec{Artifact: artSpec, RemoteDir: cfg.ClusterDeployDir}
		spec.InstallDir = installDir
	}
	app := yugabyted.New(nodes, spec, yugabyted.WithLogf(logf))
	if err := app.Start(ctx); err != nil {
		return nil, nil, fmt.Errorf("start yugabyted cluster: %w", err)
	}
	td.push("destroy yugabyted cluster", func(ctx context.Context) error { return app.Destroy(ctx, discardSink{}) })
	if err := app.Wait(ctx); err != nil {
		return nil, nil, fmt.Errorf("yugabyted cluster never became ready: %w", err)
	}
	return ybbacking.FromYugabyted(app), plan, nil
}

// provisionRawYugabyteOnAWS: vm host → a raw yb-master + yb-tserver cluster.
func (cfg Config) provisionRawYugabyteOnAWS(ctx context.Context, t *core.T, td *teardownStack, logf func(string, ...any)) (ybbacking.BackingStore, *reap.Plan, error) {
	host, addrs, plan, err := cfg.provisionVMHost(ctx, t, td)
	if err != nil {
		return nil, nil, err
	}
	execs := host.Nodes()
	nodes := make([]yugabyte.Node, len(execs))
	for i := range execs {
		nodes[i] = yugabyte.Node{Exec: execs[i], Advertise: addrs[i]}
	}
	spec := yugabyte.Spec{
		InstallDir: cfg.ClusterInstallDir,
		DataDir:    cfg.ClusterDataDir,
		RF:         cfg.clusterRF(len(nodes)),
		YSQLPort:   cfg.ysqlPort(),
	}
	artSpec, installDir, deployNow, err := cfg.clusterDeploy(t.Options)
	if err != nil {
		return nil, nil, err
	}
	if deployNow {
		spec.Deploy = &yugabyte.DeploySpec{Artifact: artSpec, RemoteDir: cfg.ClusterDeployDir}
		spec.InstallDir = installDir
	}
	app := yugabyte.New(nodes, spec, yugabyte.WithLogf(logf))
	if err := app.Start(ctx); err != nil {
		return nil, nil, fmt.Errorf("start raw yugabyte cluster: %w", err)
	}
	td.push("destroy raw yugabyte cluster", func(ctx context.Context) error { return app.Destroy(ctx, discardSink{}) })
	if err := app.Wait(ctx); err != nil {
		return nil, nil, fmt.Errorf("raw yugabyte cluster never became ready: %w", err)
	}
	return ybbacking.FromYugabyte(app), plan, nil
}

// provisionYBK8s: deploy a YugabyteDB cluster INTO the k8s cluster (apps/ybk8s)
// and expose it as a co-located backing target. The framework's layer-1
// workload reaches YB over the tailnet (the tserver pod IPs, via ConnInfo /
// HostPortsCSV), while the amp deploy step points the amp controller +
// endpoints at the in-cluster tserver Service DNS (ybbacking.Colocated's
// ControllerAddrs) so amp↔YB traffic stays in-cluster. The ybk8s Teardown
// (namespace + PVCs) is registered the instant the deployer exists, so a
// mid-Deploy failure still reaps everything.
func (cfg Config) provisionYBK8s(ctx context.Context, t *core.T, td *teardownStack, logf func(string, ...any)) (ybbacking.BackingStore, *reap.Plan, error) {
	runTag := t.ResourceSuffix()
	k8sCfg := ybk8s.FromConfig(t.Options)
	secrets, err := core.NewSecrets(t.Options)
	if err != nil {
		return nil, nil, fmt.Errorf("secrets: %w", err)
	}
	d := ybk8s.New(k8sCfg, runTag).WithLogf(t.Masker().Logf(logf)).WithSecretResolver(secrets.Value)
	td.push("teardown yb-in-k8s (namespace + PVCs)", func(ctx context.Context) error { return d.Teardown(ctx) })

	info, err := d.Deploy(ctx)
	if err != nil {
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
	inCluster := []string{info.InClusterHostPorts}
	// Reap plan: delete the deploy's namespace (ampqa-yb-<runTag>) + PVCs on the
	// same kubecontext via ybk8s' own Teardown.
	plan := &reap.Plan{Namespaces: []reap.NSReap{{
		Kind:        reap.KindYBK8s,
		KubeContext: k8sCfg.KubeContext,
		Kubeconfig:  k8sCfg.Kubeconfig,
		Namespace:   d.Namespace(),
	}}}
	return ybbacking.FromYBK8s(conn, info.TailnetAddrs, inCluster), plan, nil
}

// provisionYBAUniverse: attach an existing YBA universe (env.universe.yba.universe)
// or, when owned with a create spec, create one; a delete cleanup is registered
// only when the universe is owned.
// It is a foundation RUN provisioner only (not a setup-only preset shape), so it
// returns no reap.Plan — its teardown always runs in-process.
func (cfg Config) provisionYBAUniverse(ctx context.Context, t *core.T, td *teardownStack, logf func(string, ...any)) (ybbacking.BackingStore, *reap.Plan, error) {
	cl := yba.New(cfg.YBAURL, cfg.YBACustomer, cfg.YBAToken, yba.WithHTTPClient(ybaHTTPClient(cfg.YBAInsecure)))

	var u yba.Universe
	if cfg.UniverseRef != "" {
		got, err := cl.GetUniverse(ctx, cfg.UniverseRef)
		if err != nil {
			return nil, nil, fmt.Errorf("attach universe %q: %w", cfg.UniverseRef, err)
		}
		u = got
	} else {
		ref, err := cl.CreateUniverseAndWait(ctx, cfg.YBACreateSpec)
		if err != nil {
			return nil, nil, fmt.Errorf("create universe: %w", err)
		}
		got, err := cl.GetUniverse(ctx, ref.ResourceUUID)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve created universe %s: %w", ref.ResourceUUID, err)
		}
		u = got
	}

	if cfg.YBAOwned {
		id := u.UniverseUUID
		td.push("delete owned universe", func(ctx context.Context) error {
			_, err := cl.DeleteUniverseAndWait(ctx, id)
			return err
		})
	}

	bs, err := ybbacking.FromYBAUniverse(u)
	if err != nil {
		return nil, nil, err
	}
	return bs, nil, nil
}

// provisionYBAProvisioned stands up an OWNED YBA + universe via env/universe's
// provision_yba mode and adapts the resulting endpoint into a backing store. The
// deploy→provider→universe flow and its guaranteed, partial-safe teardown live
// in the env; this registers that teardown on the foundation stack the instant
// provisioning returns, so the YBA + universe are always reaped.
func (cfg Config) provisionYBAProvisioned(ctx context.Context, t *core.T, td *teardownStack, logf func(string, ...any)) (ybbacking.BackingStore, *reap.Plan, error) {
	secrets, err := core.NewSecrets(t.Options)
	if err != nil {
		return nil, nil, fmt.Errorf("secrets: %w", err)
	}
	ucfg := universe.FromConfig(t.Options)
	ucfg.Mode = universe.ModeProvisionYBA
	prov := universe.New(ucfg,
		universe.WithLogf(t.Masker().Logf(logf)),
		universe.WithProvisionYBA(t.Options, secrets.Value))

	env, err := prov.Provision(ctx, core.EnvSpec{Sharing: core.Exclusive, Shape: core.Universe}, t.Seed.Fork("yba"))
	if err != nil {
		return nil, nil, fmt.Errorf("provision owned yba universe: %w", err)
	}
	td.push("teardown provisioned yba (universe + control plane)", func(ctx context.Context) error {
		return prov.Teardown(ctx, env)
	})

	acc, ok := env.(interface {
		PGEndpoint() pg.ConnInfo
		NodeAddrs() []string
		ProvisionedHandles() universe.ProvisionedHandles
	})
	if !ok {
		return nil, nil, fmt.Errorf("provisioned yba env does not expose a YSQL endpoint (%T)", env)
	}
	addrs := acc.NodeAddrs()
	return ybbacking.Static{
		Conn:  acc.PGEndpoint(),
		Addrs: addrs,
		Desc:  fmt.Sprintf("provisioned YBA universe (%d live tservers, primary %s)", len(addrs), acc.PGEndpoint().Addr()),
	}, ybaReapPlan(acc.ProvisionedHandles()), nil
}

// ybaReapPlan builds the separate-process reap plan for a provision_yba env: the
// YBA control plane is ALWAYS a ybastack k8s namespace, and the universe backing
// is EITHER a k8s namespace (provider_type kubernetes — reaped by discovering the
// YBA-created universe namespace) OR cloud nodes (aws/gcp — reaped by ownership
// tag). Returns nil when there is nothing identifiable to reap (no YBA namespace).
func ybaReapPlan(h universe.ProvisionedHandles) *reap.Plan {
	if h.YBANamespace == "" {
		return nil
	}
	p := &reap.Plan{Namespaces: []reap.NSReap{{
		Kind:        reap.KindYBAStack,
		KubeContext: h.KubeContext,
		Kubeconfig:  h.Kubeconfig,
		Namespace:   h.YBANamespace,
		HelmRelease: h.HelmRelease,
		CRBs:        h.CRBs,
	}}}
	switch h.ProviderType {
	case "kubernetes":
		p.Namespaces = append(p.Namespaces, reap.NSReap{
			Kind:        reap.KindDiscover,
			KubeContext: h.KubeContext,
			Kubeconfig:  h.Kubeconfig,
			Namespace:   h.UniverseName,
		})
	case "aws", "gcp":
		p.Cloud = &reap.CloudReap{Cloud: h.ProviderType, RunTag: h.RunTag, Region: h.Region}
	}
	return p
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
