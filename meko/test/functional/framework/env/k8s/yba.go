package k8s

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/yba"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/ybastack"
	"github.com/yugabyte/meko/meko/test/functional/framework/apps/ybauniverse"
	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/env/reap"
	execk8s "github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
	"github.com/yugabyte/meko/meko/test/functional/framework/universe"
)

const ybaPlanKey = "yba"

// ybaRun is the one YBA a run uses, stood up when the first yba universe is
// asked for and removed when the provider closes.
type ybaRun struct {
	mu       sync.Mutex
	deployer *ybastack.Deployer
	prov     *ybauniverse.Provider
}

type ybaCreator struct {
	prov *ybauniverse.Provider
	ctx  string
	tag  string
}

func (c ybaCreator) Create(ctx context.Context, spec universe.Spec) (universe.Universe, error) {
	u, err := c.prov.WithRunTag(c.tag).Create(ctx, spec)
	if u == nil {
		return nil, err
	}
	return u, err
}

// Reaps is the YBA's universe namespaces, which YBA names after the universe.
func (c ybaCreator) Reaps(name string) []reap.NSReap {
	return []reap.NSReap{{Kind: reap.KindDiscover, KubeContext: c.ctx, Namespace: name + "-" + c.tag}}
}

func (p *Provider) validateYBA() error {
	if p.cfg.core == nil {
		return nil
	}
	if p.cfg.core.GetString("versions.yugabytedb", "") == "" && p.cfg.core.GetString("db.version", "") == "" {
		return errors.New("db.provider=yba needs db.version or versions.yugabytedb, a release YBA can run")
	}
	return nil
}

// ybaCreator deploys the run's YBA on first use.
func (p *Provider) ybaCreator(ctx context.Context, tag string) (creator, error) {
	r, err := p.ybaProvider(ctx)
	if err != nil {
		return nil, err
	}
	return ybaCreator{prov: r, ctx: p.cfg.Context, tag: tag}, nil
}

func (p *Provider) ybaProvider(ctx context.Context) (*ybauniverse.Provider, error) {
	p.mu.Lock()
	if p.yba == nil {
		p.yba = &ybaRun{}
	}
	r := p.yba
	p.mu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.prov != nil {
		return r.prov, nil
	}
	c := p.cfg.core.Derive(map[string]string{
		"apps.ybastack.kubecontext":       p.cfg.Context,
		"apps.yba.k8s.use_in_cluster":     "bool:true",
		"apps.yba.k8s.pull_secret_secret": "mekoqa-quay-pull",
	})
	secrets := p.ybaSecrets()
	resolve := func(name string) (string, error) {
		if v, ok := secrets[name]; ok {
			return v, nil
		}
		s, err := core.NewSecrets(c)
		if err != nil {
			return "", err
		}
		return s.Value(name)
	}
	cfg := ybastack.FromConfig(c)
	tag := p.cfg.RunID
	d := ybastack.New(cfg, tag, resolve).WithLogf(p.logf)
	p.trackBacking(ybaPlanKey, []reap.NSReap{{Kind: reap.KindYBAStack, KubeContext: p.cfg.Context, Namespace: d.Namespace()}})
	p.log("k8s: standing up the run's YBA in %s", d.Namespace())
	info, err := d.Deploy(ctx)
	if err != nil {
		tctx, cancel := context.WithTimeout(context.Background(), core.ResolveTeardownTimeout(p.cfg.core))
		defer cancel()
		if terr := d.Teardown(tctx); terr != nil {
			return nil, fmt.Errorf("deploy YBA: %w (teardown left it in the reap plan: %v)", err, terr)
		}
		p.untrackBacking(ybaPlanKey)
		return nil, fmt.Errorf("deploy YBA: %w", err)
	}
	p.trackBacking(ybaPlanKey, []reap.NSReap{{Kind: reap.KindYBAStack, KubeContext: p.cfg.Context, Namespace: info.Namespace, HelmRelease: info.HelmRelease, CRBs: info.ClusterRoleBindings}})
	hc := &http.Client{Timeout: 60 * time.Second, Transport: info.Transport}
	if info.Transport == nil {
		hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // YBA is self-signed
	}
	cl := yba.New(info.URL, info.CustomerUUID, info.APIToken, yba.WithHTTPClient(hc), yba.WithLogf(p.logf))
	ks, err := yba.BuildK8sProviderSpec(c, resolve)
	if err != nil {
		_ = d.Teardown(context.Background())
		return nil, err
	}
	// db.version is declared empty, so GetString's fallback never fires for it.
	version := c.GetString("db.version", "")
	if version == "" {
		version = c.GetString("versions.yugabytedb", "")
	}
	r.deployer = d
	r.prov = &ybauniverse.Provider{
		Client: cl, K8sProvider: ks, Version: version, Logf: p.logf,
		Kube: execk8s.New(execk8s.Config{Context: p.cfg.Context}),
	}
	return r.prov, nil
}

// ybaSecrets hands YBA the stack's Quay credential in the two shapes it wants:
// the dockerconfigjson its own pods pull with, and the pull secret it copies
// into the universe namespaces.
func (p *Provider) ybaSecrets() map[string]string {
	u, t := p.cfg.Stack.QuayUser, p.cfg.Stack.QuayToken
	if u == "" || t == "" {
		return nil
	}
	auth := base64.StdEncoding.EncodeToString([]byte(u + ":" + t))
	dcj, _ := json.Marshal(map[string]any{"auths": map[string]any{"quay.io": map[string]string{"username": u, "password": t, "auth": auth}}})
	secret := fmt.Sprintf("apiVersion: v1\nkind: Secret\nmetadata:\n  name: quay-secret\ntype: kubernetes.io/dockerconfigjson\ndata:\n  .dockerconfigjson: %s\n", base64.StdEncoding.EncodeToString(dcj))
	return map[string]string{ybastack.DefaultQuaySecret: string(dcj), "mekoqa-quay-pull": secret}
}

// closeYBA removes the run's YBA, if one was stood up.
func (p *Provider) closeYBA() error {
	p.mu.Lock()
	r := p.yba
	p.yba = nil
	p.mu.Unlock()
	if r == nil || r.deployer == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), reapTimeout)
	defer cancel()
	if err := r.deployer.Teardown(ctx); err != nil {
		return fmt.Errorf("remove the run's YBA: %w", err)
	}
	p.untrackBacking(ybaPlanKey)
	return nil
}
