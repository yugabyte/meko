// Package ybauniverse is the "yba" universe provider: universes created and
// operated through the YugabyteDB Anywhere API, on its Kubernetes provider. One
// YBA serves a run, and each universe is created in the body of a test and
// deleted after it.
package ybauniverse

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/yba"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/ybbacking"
	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
	"github.com/yugabyte/meko/meko/test/functional/framework/kubemeta"
	"github.com/yugabyte/meko/meko/test/functional/framework/universe"
)

const (
	tserverContainer = "yb-tserver"
	ysqlsh           = "/home/yugabyte/bin/ysqlsh"
	createTimeout    = 45 * time.Minute
)

// Provider creates universes on one YBA.
type Provider struct {
	Client *yba.Client
	// K8sProvider is registered on first use. Region and zones come from it.
	K8sProvider yba.K8sProviderSpec
	// Version is the DB release when a spec names none. It must be a release YBA
	// knows, or one it can import for kubernetes.
	Version string
	Kube    *k8s.Kubectl
	RunTag  string
	Logf    func(string, ...any)

	reg *registration
}

type registration struct {
	mu   sync.Mutex
	uuid string
}

var regInit sync.Mutex

func (p *Provider) registration() *registration {
	regInit.Lock()
	defer regInit.Unlock()
	if p.reg == nil {
		p.reg = &registration{}
	}
	return p.reg
}

// WithRunTag is p for one test's universes. The copies share one provider
// registration, since YBA refuses a second provider of the same name.
func (p *Provider) WithRunTag(tag string) *Provider {
	p.registration()
	c := *p
	c.RunTag = tag
	return &c
}

func (p *Provider) ensureProvider(ctx context.Context) error {
	r := p.registration()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.uuid != "" {
		return nil
	}
	id, err := p.Client.EnsureK8sProvider(ctx, p.K8sProvider)
	if err != nil {
		return fmt.Errorf("register the kubernetes provider: %w", err)
	}
	r.uuid = id
	return nil
}

func (p *Provider) providerUUID() string {
	r := p.registration()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.uuid
}

func (p *Provider) log(f string, a ...any) {
	if p.Logf != nil {
		p.Logf(f, a...)
	}
}

func ceilInt(f float64) int { return int(math.Ceil(f)) }

// BuildSpec maps a universe spec onto YBA's create request.
func (p *Provider) BuildSpec(s universe.Spec) (yba.UniverseSpec, error) {
	if len(s.Clusters) != 1 {
		return yba.UniverseSpec{}, universe.Unsupportedf(universe.YBA, "read replica clusters at create", "create the primary, then add the replica")
	}
	c := s.Primary()
	if s.Image != "" {
		return yba.UniverseSpec{}, universe.Unsupportedf(universe.YBA, "an image reference", "YBA runs the release named by a version from its provider's image registry")
	}
	version := s.Version
	if version == "" {
		version = p.Version
	}
	region := ""
	if len(p.K8sProvider.Regions) > 0 {
		region = p.K8sProvider.Regions[0].Code
	}
	us := yba.UniverseSpec{
		NamePrefix:        s.Name,
		RunTag:            p.RunTag,
		ProviderUUID:      p.providerUUID(),
		ProviderCode:      "kubernetes",
		Region:            region,
		NumNodes:          c.Tservers(),
		ReplicationFactor: c.RF,
		YBSoftwareVersion: version,
		VolumeSizeGB:      c.Node.DiskGiB,
		VolumeCount:       1,
		K8sStorageClass:   c.Node.StorageClass,
		MasterCPUCores:    ceilInt(c.Node.CPU),
		MasterMemGiB:      ceilInt(c.Node.MemoryGiB),
		TServerCPUCores:   ceilInt(c.Node.CPU),
		TServerMemGiB:     ceilInt(c.Node.MemoryGiB),
		EnableYbc:         true,
		MasterGFlags:      merge(s.MasterGflags, c.MasterGflags),
		TServerGFlags:     merge(s.TServerGflags, c.TServerGflags),

		EnableNodeToNodeEncrypt:   s.TLS.Nodes,
		EnableClientToNodeEncrypt: s.TLS.Clients,
	}
	if len(c.Zones()) > 1 {
		rp := yba.RegionPlacement{Code: region}
		for _, z := range c.Zones() {
			rp.Zones = append(rp.Zones, yba.ZonePlacement{Name: universe.ZoneName(z), Nodes: c.Placement[z]})
		}
		us.Placement = []yba.RegionPlacement{rp}
	}
	return us, nil
}

func merge(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// Create builds the universe and returns it once YBA reports it healthy.
func (p *Provider) Create(ctx context.Context, spec universe.Spec) (*Universe, error) {
	spec = spec.WithDefaults()
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := p.ensureProvider(ctx); err != nil {
		return nil, err
	}
	us, err := p.BuildSpec(spec)
	if err != nil {
		return nil, err
	}
	if len(us.Placement) == 0 {
		codes := []string{us.Region}
		if us.RegionUUIDs, err = p.Client.RegionUUIDsForCodes(ctx, p.providerUUID(), codes...); err != nil {
			return nil, err
		}
	} else if us.Placement, err = p.Client.ResolvePlacement(ctx, p.providerUUID(), us.Placement); err != nil {
		return nil, err
	} else {
		for _, r := range us.Placement {
			us.RegionUUIDs = append(us.RegionUUIDs, r.UUID)
		}
	}
	if err := p.Client.EnsureReleaseK8s(ctx, us.YBSoftwareVersion); err != nil {
		return nil, fmt.Errorf("make release %s available: %w", us.YBSoftwareVersion, err)
	}
	ref, err := p.Client.CreateUniverseFromSpec(ctx, us)
	if err != nil {
		return nil, err
	}
	u := &Universe{p: p, spec: spec, id: ref.ResourceUUID}
	if _, err := p.Client.WaitTask(ctx, ref.TaskUUID); err != nil {
		return u, fmt.Errorf("create universe %s: %w", spec.Name, err)
	}
	if err := p.Client.WaitUniverseReady(ctx, u.id, createTimeout); err != nil {
		return u, err
	}
	if err := u.refresh(ctx); err != nil {
		return u, err
	}
	u.claimNamespace(ctx)
	return u, nil
}

// claimNamespace labels the namespace YBA created for the universe as the
// run's, so the janitor reaps it once the run's YBA is gone. Without the labels
// a kept or killed run's universe ran on with nothing left to delete it.
func (u *Universe) claimNamespace(ctx context.Context) {
	if u.p.Kube == nil || u.ns == "" || !strings.Contains(u.ns, u.spec.Name) {
		return
	}
	labels := kubemeta.TestLabels(map[string]string{
		kubemeta.ManagedByLabel: kubemeta.ManagedByValue,
		kubemeta.RunLabel:       kubemeta.LabelValue(u.p.RunTag),
	})
	for k, v := range labels {
		if err := u.p.Kube.LabelNamespace(ctx, u.ns, k, v); err != nil && u.p.Logf != nil {
			u.p.Logf("ybauniverse: label %s %s=%s: %v", u.ns, k, v, err)
		}
	}
}

// Universe is a universe YBA runs.
type Universe struct {
	p       *Provider
	spec    universe.Spec
	id      string
	ns      string
	pod     string
	svcHost string
	svcPort int
	version string

	xcMu    sync.Mutex
	xcStore *universe.S3
	drs     map[string]string
}

var (
	_ universe.Universe    = (*Universe)(nil)
	_ universe.Gflags      = (*Universe)(nil)
	_ universe.Upgrades    = (*Universe)(nil)
	_ universe.Backups     = (*Universe)(nil)
	_ universe.PITR        = (*Universe)(nil)
	_ universe.NodeControl = (*Universe)(nil)
)

func (u *Universe) Name() string                { return u.spec.Name }
func (u *Universe) Spec() universe.Spec         { return u.spec }
func (u *Universe) Provider() universe.Provider { return universe.YBA }
func (u *Universe) Namespace() string           { return u.ns }
func (u *Universe) ID() string                  { return u.id }
func (u *Universe) ClusterAddr() (string, int)  { return u.svcHost, u.svcPort }

// Conn has no host: a kubernetes universe's pods are reached through kubectl.
func (u *Universe) Conn() universe.ConnInfo {
	return universe.ConnInfo{Port: u.svcPort, User: "yugabyte", Database: "yugabyte", SSLMode: "disable"}
}

func (u *Universe) refresh(ctx context.Context) error {
	yu, err := u.p.Client.GetUniverse(ctx, u.id)
	if err != nil {
		return err
	}
	store, err := ybbacking.FromYBAUniverse(yu)
	if err != nil {
		return err
	}
	col, ok := store.(ybbacking.Colocated)
	if !ok || len(col.InClusterAddrs) == 0 {
		return fmt.Errorf("universe %s has no kubernetes tserver address", u.spec.Name)
	}
	u.ns, u.pod = col.KubeColocation()
	u.svcHost, u.svcPort, _ = ybbacking.ServiceFromPodDNS(col.InClusterAddrs[0])
	if u.svcPort == 0 {
		u.svcPort = 5433
	}
	u.version = yu.Version()
	return nil
}

func (u *Universe) kube() *k8s.Kubectl { return u.p.Kube.InNamespace(u.ns) }

// Nodes lists the universe's pods.
func (u *Universe) Nodes(ctx context.Context) ([]universe.Node, error) {
	yu, err := u.p.Client.GetUniverse(ctx, u.id)
	if err != nil {
		return nil, err
	}
	var out []universe.Node
	replica := map[string]bool{}
	for _, c := range yu.UniverseDetails.Clusters {
		replica[c.UUID] = c.ClusterType == "ASYNC"
	}
	for _, n := range yu.UniverseDetails.NodeDetailsSet {
		cluster := universe.Primary
		if replica[n.PlacementUUID] {
			cluster = universe.ReadReplica
		}
		out = append(out, universe.Node{
			Name: strings.Split(n.CloudInfo.PrivateIP, ".")[0], Cluster: cluster, Zone: n.CloudInfo.Zone, Tserver: n.IsTserver, Ready: strings.EqualFold(n.State, "Live"),
		})
	}
	return out, nil
}

// SQL runs one statement in a tserver pod, the statement on stdin.
func (u *Universe) SQL(ctx context.Context, node, database, statement string) (string, error) {
	if node == "" {
		node = u.pod
	}
	if database == "" {
		database = "yugabyte"
	}
	cmd := fmt.Sprintf(`%s -h "$(hostname -f)" -p %d -U yugabyte -d '%s' -v ON_ERROR_STOP=1 -tA`, ysqlsh, u.svcPort, database)
	res, err := u.kube().ForPod(node, tserverContainer).RunStdin(ctx, strings.NewReader(statement), cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("ysqlsh in %s exited %d: %s", node, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

// Teardown deletes the universe through YBA.
func (u *Universe) Teardown(ctx context.Context) error {
	if u.id == "" {
		return nil
	}
	_, err := u.p.Client.DeleteUniverseAndWait(ctx, u.id)
	return err
}

func (u *Universe) SetGflags(ctx context.Context, master, tserver map[string]string) error {
	return u.p.Client.UpgradeGflagsAndWait(ctx, u.id, master, tserver, yba.UpgradeRolling, 0)
}

func (u *Universe) Upgrade(ctx context.Context, version string) error {
	if err := u.p.Client.EnsureReleaseK8s(ctx, version); err != nil {
		return err
	}
	if err := u.p.Client.UpgradeSoftwareAndWait(ctx, u.id, version, yba.UpgradeRolling, 0); err != nil {
		return err
	}
	need, err := u.p.Client.FinalizeRequired(ctx, u.id, version)
	if err != nil || !need {
		return err
	}
	ref, err := u.p.Client.FinalizeUpgrade(ctx, u.id)
	if err != nil {
		return err
	}
	_, err = u.p.Client.WaitTask(ctx, ref.TaskUUID)
	return err
}

func (u *Universe) KillNode(ctx context.Context, node string) error {
	return u.kube().DeleteNow(ctx, node)
}

func (u *Universe) StopNode(context.Context, string) error {
	return universe.Unsupportedf(universe.YBA, "stop a node", "a kubernetes universe's pod cannot be stopped without YBA restarting it, use KillNode")
}

func (u *Universe) StartNode(context.Context, string) error {
	return universe.Unsupportedf(universe.YBA, "start a node", "see StopNode")
}

func (u *Universe) storageConfig(ctx context.Context, s universe.S3) (string, error) {
	data := map[string]any{
		"BACKUP_LOCATION":       "s3://" + s.Bucket,
		"AWS_ACCESS_KEY_ID":     s.AccessKey,
		"AWS_SECRET_ACCESS_KEY": s.SecretKey,
		"AWS_HOST_BASE":         s.Endpoint,
		"PATH_STYLE_ACCESS":     true,
	}
	return u.p.Client.EnsureStorageConfig(ctx, "S3", "mekoqa-"+s.Bucket, data)
}

func (u *Universe) Backup(ctx context.Context, database string, to universe.S3) (universe.Backup, error) {
	cfg, err := u.storageConfig(ctx, to)
	if err != nil {
		return universe.Backup{}, err
	}
	b, err := u.p.Client.CreateBackupAndWait(ctx, yba.BackupSpec{UniverseUUID: u.id, StorageConfigUUID: cfg, Keyspace: database})
	if err != nil {
		return universe.Backup{}, err
	}
	return universe.Backup{ID: b.BackupUUID, Database: database, Location: b.Location(database), At: time.Now()}, nil
}

func (u *Universe) Restore(ctx context.Context, b universe.Backup, from universe.S3) error {
	cfg, err := u.storageConfig(ctx, from)
	if err != nil {
		return err
	}
	return u.p.Client.RestoreBackupAndWait(ctx, u.id, cfg, b.Database, b.Location)
}

func (u *Universe) EnablePITR(ctx context.Context, database string, retention time.Duration) error {
	_, err := u.p.Client.CreatePitrConfigAndWait(ctx, u.id, database, retention, time.Hour)
	return err
}

func (u *Universe) RecoverTo(ctx context.Context, database string, at time.Time) error {
	cfgs, err := u.p.Client.ListPitrConfigs(ctx, u.id)
	if err != nil {
		return err
	}
	for _, c := range cfgs {
		if c.DBName == database {
			return u.p.Client.RestorePitrAndWait(ctx, u.id, c.UUID, at.UnixMilli())
		}
	}
	return fmt.Errorf("no PITR config for %s: call EnablePITR first", database)
}
