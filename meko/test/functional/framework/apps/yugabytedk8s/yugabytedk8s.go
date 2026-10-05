// Package yugabytedk8s is the "yugabyted" universe provider: yugabyted in pods,
// one StatefulSet per zone and cluster, in a namespace of its own. The image
// has to ship yugabyted, which quay.io/yugabyte/meko-yugabytedb does, along with
// the extensions meko needs.
package yugabytedk8s

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
	"github.com/yugabyte/meko/meko/test/functional/framework/kubemeta"
	"github.com/yugabyte/meko/meko/test/functional/framework/universe"
)

const (
	// ManagedByLabel marks a namespace this provider made, so teardown refuses any other.
	ManagedByLabel = "mekoqa-managed-by"
	ManagedByValue = "yugabytedk8s"

	DefaultNamespacePrefix = "mekoqa-yu"
	DefaultReadyTimeout    = 20 * time.Minute
)

// Config configures the provider.
type Config struct {
	KubeContext string
	Kubeconfig  string

	NamespacePrefix string
	// Image is used when a spec names neither an image nor a version. Its
	// repository is what a spec's Version is applied to.
	Image string
	// PullUser and PullToken are the registry credential for quay.io. Empty means
	// the image is pulled anonymously.
	PullUser  string
	PullToken string

	RunID string
	Owner string
	TTL   time.Duration

	ReadyTimeout time.Duration
	// ExtraGflags are laid under every universe's own flags. A meko stack sets
	// the mage flags here.
	Logf func(string, ...any)
}

// Provider creates universes.
type Provider struct {
	cfg     Config
	cluster *k8s.Kubectl

	mu      sync.Mutex
	regions map[string]string
	now     func() time.Time
}

// New builds a provider on the real kubectl.
func New(cfg Config) *Provider {
	if cfg.NamespacePrefix == "" {
		cfg.NamespacePrefix = DefaultNamespacePrefix
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = DefaultReadyTimeout
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	return &Provider{
		cfg:     cfg,
		cluster: k8s.New(k8s.Config{Context: cfg.KubeContext}).WithRunner(k8s.KubeconfigRunner(cfg.Kubeconfig)),
		now:     time.Now,
	}
}

// WithRunner replaces the kubectl runner, for tests.
func (p *Provider) WithRunner(r k8s.Runner) *Provider {
	p.cluster = p.cluster.WithRunner(r)
	return p
}

func (p *Provider) log(format string, args ...any) {
	if p.cfg.Logf != nil {
		p.cfg.Logf(format, args...)
	}
}

// Namespace is where a universe of this name will run.
func (p *Provider) Namespace(name string) string {
	parts := []string{p.cfg.NamespacePrefix}
	if p.cfg.RunID != "" {
		parts = append(parts, p.cfg.RunID)
	}
	return strings.Join(append(parts, name), "-")
}

// zoneRegions maps each zone the cluster's nodes are in to its region.
func (p *Provider) zoneRegions(ctx context.Context) map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.regions != nil {
		return p.regions
	}
	out, err := p.cluster.GetSelector(ctx, "nodes", "",
		`{range .items[*]}{.metadata.labels.topology\.kubernetes\.io/zone}{"\t"}{.metadata.labels.topology\.kubernetes\.io/region}{"\n"}{end}`)
	regions := map[string]string{}
	if err == nil {
		for _, line := range strings.Split(out, "\n") {
			z, r, _ := strings.Cut(strings.TrimSpace(line), "\t")
			if z != "" {
				regions[z] = r
			}
		}
	}
	p.regions = regions
	return regions
}

func (p *Provider) imageFor(s universe.Spec) string {
	switch {
	case s.Image != "":
		return s.Image
	case s.Version != "":
		repo := p.cfg.Image
		if i := strings.LastIndexByte(repo, ':'); i > strings.LastIndexByte(repo, '/') {
			repo = repo[:i]
		}
		return repo + ":" + s.Version
	}
	return p.cfg.Image
}

// check refuses what yugabyted cannot do, with the reason.
func check(s universe.Spec) error {
	if s.TLS.Nodes || s.TLS.Clients {
		return universe.Unsupportedf(universe.Yugabyted, "tls", "the pods run yugabyted insecure, use the yba provider for a TLS universe")
	}
	if s.YSQLAuth {
		return universe.Unsupportedf(universe.Yugabyted, "ysql auth", "the pods run with YSQL auth off, use the yba provider")
	}
	for _, c := range s.Clusters {
		if c.Type == universe.Primary {
			if len(c.Masters.Placement) > 0 || (c.Masters.Count != 0 && c.Masters.Count != c.RF) {
				return universe.Unsupportedf(universe.Yugabyted, "master placement", "yugabyted runs as many masters as the replication factor, %d here", c.RF)
			}
		}
	}
	return nil
}

// Create builds the universe and returns it once every node serves YSQL.
func (p *Provider) Create(ctx context.Context, spec universe.Spec) (*Universe, error) {
	spec = spec.WithDefaults()
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if p.imageFor(spec) == "" {
		return nil, fmt.Errorf("universe %s: no image (env.mekostack.images.database or db.image)", spec.Name)
	}
	if err := check(spec); err != nil {
		return nil, err
	}
	u := &Universe{p: p, spec: spec, ns: p.Namespace(spec.Name), image: p.imageFor(spec), regions: p.zoneRegions(ctx)}
	u.kube = k8s.New(k8s.Config{Context: p.cfg.KubeContext, Namespace: u.ns}).WithRunner(p.cluster.Runner())
	if err := u.create(ctx); err != nil {
		return u, err
	}
	return u, nil
}

func (p *Provider) namespaceLabels() map[string]string {
	extra := map[string]string{ManagedByLabel: ManagedByValue}
	if v := kubemeta.LabelValue(p.cfg.RunID); v != "" {
		extra[kubemeta.RunLabel] = v
	}
	if v := kubemeta.LabelValue(p.cfg.Owner); v != "" {
		extra[kubemeta.OwnerLabel] = v
	}
	return kubemeta.TestLabels(extra)
}

func (p *Provider) pullSecretManifest(ns string) (string, error) {
	if p.cfg.PullUser == "" || p.cfg.PullToken == "" {
		return "", nil
	}
	auth := base64.StdEncoding.EncodeToString([]byte(p.cfg.PullUser + ":" + p.cfg.PullToken))
	cfg, err := json.Marshal(map[string]any{"auths": map[string]any{"quay.io": map[string]string{
		"username": p.cfg.PullUser, "password": p.cfg.PullToken, "auth": auth}}})
	if err != nil {
		return "", err
	}
	return render(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/dockerconfigjson",
		"metadata": map[string]any{"name": pullSecret, "namespace": ns},
		"data":     map[string]any{".dockerconfigjson": base64.StdEncoding.EncodeToString(cfg)},
	})
}

// Reap deletes a universe namespace from a process that holds nothing of it. It
// refuses a namespace this provider did not label.
func Reap(ctx context.Context, kubeContext, kubeconfig, namespace string) error {
	kube := k8s.New(k8s.Config{Context: kubeContext}).WithRunner(k8s.KubeconfigRunner(kubeconfig))
	return deleteNamespace(ctx, kube, namespace)
}

func deleteNamespace(ctx context.Context, cluster *k8s.Kubectl, ns string) error {
	label, err := cluster.Get(ctx, "namespace", ns, "{.metadata.labels."+strings.ReplaceAll(ManagedByLabel, ".", `\.`)+"}")
	if err != nil {
		if k8s.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read the label of namespace %s: %w", ns, err)
	}
	if label != ManagedByValue {
		return fmt.Errorf("refusing to delete namespace %s: it is not a yugabytedk8s namespace (%s=%q)", ns, ManagedByLabel, label)
	}
	return cluster.DeleteNamespace(ctx, ns)
}
