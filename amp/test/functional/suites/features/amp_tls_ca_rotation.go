// Rehearses the CA rotation in runbooks/tls.md on a stack the run stood up on
// cert-manager certificates: a second root joins the trust bundle, the issuer
// moves to it, every leaf is renewed and the database rolled onto its own, and
// the endpoint keeps answering SQL — its daemons reconnecting with leaves from
// the new root and no pod restart. The issuer and roots ConfigMap are restored
// at teardown.
//
// It rewrites the configured issuer's root, which is cluster-wide trust, so it
// runs only when functional.tls_rotation.enabled says the run owns that issuer.
package features

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/controller"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	tlsRotationRows     = 100
	tlsRotationRollout  = 20 * time.Minute
	tlsRotationBundleUp = 3 * time.Minute
	tlsRotationVerify   = 5 * time.Minute
)

func init() {
	core.Register(core.Test{
		Name:        "features/amp_tls_ca_rotation",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"tls"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Rotate the certificate authority under a TLS stack: trust a second root, repoint the issuer, renew every leaf and roll the database, then assert the endpoint still answers SQL and its daemons authenticate to YugabyteDB with certificates from the new root, without a restart.",
		Params:      []string{"functional", "env.ampstack", "apps.ybk8s"},
		Timeout:     90 * time.Minute,
		Run:         runAmpTLSCARotation,
	})
}

// tlsRotationConfig is functional.tls_rotation.* plus the issuer and bundle
// names the TLS stack was configured with.
type tlsRotationConfig struct {
	Enabled         bool
	CertManagerNS   string
	BootstrapIssuer string
	RootsConfigMap  string
	RenewTimeout    time.Duration
	IssuerKind      string
	IssuerName      string
	BundleConfigMap string
}

func tlsRotationFromConfig(c *core.Config) tlsRotationConfig {
	return tlsRotationConfig{
		Enabled:         c.GetBool("functional.tls_rotation.enabled", false),
		CertManagerNS:   c.GetString("functional.tls_rotation.cert_manager_namespace", "cert-manager"),
		BootstrapIssuer: c.GetString("functional.tls_rotation.bootstrap_issuer", "amp-ca-bootstrap"),
		RootsConfigMap:  c.GetString("functional.tls_rotation.roots_configmap", "amp-ca-roots"),
		RenewTimeout:    c.GetDuration("functional.tls_rotation.renew_timeout", 10*time.Minute),
		IssuerKind:      c.GetString("apps.ybk8s.tls.issuer_kind", "ClusterIssuer"),
		IssuerName:      c.GetString("apps.ybk8s.tls.issuer_name", "amp-ca"),
		BundleConfigMap: c.GetString("env.ampstack.tls.ca_configmap", "amp-ca-bundle"),
	}
}

// defaultRotationIssuerName is the shared issuer every amp stack in a cluster
// issues from. The rehearsal must not be pointed at it.
const defaultRotationIssuerName = "amp-ca"

// gate returns a skip reason unless the run is a self-provisioned ybk8s TLS
// stack whose issuer it is allowed to rotate.
func (cfg tlsRotationConfig) gate(c *core.Config, env core.Environment) (string, bool) {
	if !cfg.Enabled {
		return "functional.tls_rotation.enabled is off: the rehearsal rewrites the configured issuer's root and roots ConfigMap, so it runs only against a trust root this run owns", false
	}
	// A ClusterIssuer is cluster-scoped and this rehearsal repoints it. The
	// scheduler's exclusivity only covers work in this run, so another stack in
	// the same cluster would keep issuing from the rotation root and be left
	// holding leaves signed by a root the teardown deletes. Refusing the shared
	// default is the only guard available here: give the run its own issuer.
	if cfg.IssuerName == "" || cfg.IssuerName == defaultRotationIssuerName {
		return fmt.Sprintf(
			"env.ampstack.tls.issuer_name is %q, the cluster-wide default: the rehearsal repoints "+
				"the issuer it is given, so it needs one created for this run",
			cfg.IssuerName), false
	}
	if _, attached := ampEnvFrom(env); attached {
		return "attached to a standing control plane: the rehearsal needs a stack of its own to rotate under", false
	}
	if !c.GetBool("apps.ybk8s.tls.enabled", false) || !c.GetBool("env.ampstack.tls.enabled", false) {
		return "needs a TLS stack: apps.ybk8s.tls.enabled and env.ampstack.tls.enabled", false
	}
	if cfg.IssuerKind != "ClusterIssuer" {
		return fmt.Sprintf("needs a ClusterIssuer backed by a CA Secret, got %s", cfg.IssuerKind), false
	}
	if _, yba := FromConfig(c).gateYBAProvisioned(c); yba {
		return "the run would pick the YBA-provisioned backing; the rehearsal covers the chart universe (ybk8s)", false
	}
	return "", true
}

func runAmpTLSCARotation(t *core.T) error {
	cfg := tlsRotationFromConfig(t.Options)
	if reason, ok := cfg.gate(t.Options, t.Env); !ok {
		t.Skip(reason)
		return nil
	}
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		r := &tlsRotationRun{a: a, cfg: cfg}
		return []phase{
			{"tls_rotation_baseline", r.baseline},
			{"tls_rotation_trust_new_root", r.trustNewRoot},
			{"tls_rotation_repoint_issuer", r.repointIssuer},
			{"tls_rotation_renew_and_roll", r.renewAndRoll},
			{"tls_rotation_verify", r.verify},
		}
	})
}

type tlsRotationRun struct {
	a   *ampFeatureRun
	cfg tlsRotationConfig

	ep      controller.Endpoint
	ampKube *k8s.Kubectl
	ybKube  *k8s.Kubectl
	cmKube  *k8s.Kubectl

	// The endpoint pod seen at baseline. The whole point of the restart check
	// is that *this* pod survives, so its identity travels with the count.
	podAtBaseline string

	newRootName string
	newRootCN   string
	newRootPEM  string
	oldIssuerCA string
	oldRootsPEM string
}

func (r *tlsRotationRun) baseline(ctx context.Context) error {
	f := r.a.f
	r.ampKube = f.ampKube()
	if r.ampKube == nil {
		return fmt.Errorf("no kube handle for the amp namespace")
	}
	ybNS := ""
	if f.ampApp != nil {
		ybNS = f.ampApp.BackingNamespace()
	}
	if ybNS == "" {
		return fmt.Errorf("the backing is not co-located in this cluster; the rehearsal rolls its StatefulSets")
	}
	r.ybKube = r.ampKube.InNamespace(ybNS)
	r.cmKube = r.ampKube.InNamespace(r.cfg.CertManagerNS)

	cl, err := f.controllerClient(ctx)
	if err != nil {
		return err
	}
	r.ep, err = createEndpointReq(ctx, cl, r.a.proj.poolID, r.a.proj.projID, r.a.proj.branchID, controller.CreateEndpointRequest{Role: roleReadWrite})
	if err != nil {
		return fmt.Errorf("create endpoint: %w", err)
	}
	c, err := f.endpointSQL(ctx, r.ep)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Exec(ctx, "CREATE TABLE tls_rotation (id int PRIMARY KEY, v text)"); err != nil {
		return fmt.Errorf("create table: %w", err)
	}
	if _, err := c.Exec(ctx, fmt.Sprintf("INSERT INTO tls_rotation SELECT g, 'r' || g FROM generate_series(1, %d) g", tlsRotationRows)); err != nil {
		return fmt.Errorf("seed rows: %w", err)
	}

	// Before touching anything, the daemons must already be logging in by
	// certificate: otherwise the rotation proves nothing.
	issuers, err := r.daemonIssuers(ctx)
	if err != nil {
		return err
	}
	if issuers == "" {
		return fmt.Errorf("no safekeeper/pageserver backend authenticates to the backing by certificate; is the stack on TLS?")
	}
	f.logf("baseline: daemon client certificates issued by %s", issuers)

	pod, err := r.ampKube.GetSelector(ctx, "pod", "app.kubernetes.io/instance="+endpointDeployName(r.ep.ID),
		"{.items[?(@.status.phase=='Running')].metadata.name}")
	if err != nil {
		return fmt.Errorf("find the endpoint pod: %w", err)
	}
	fields := strings.Fields(pod)
	if len(fields) != 1 {
		return fmt.Errorf("want exactly one running endpoint pod at baseline, got %q", pod)
	}
	r.podAtBaseline = fields[0]
	f.logf("baseline: endpoint pod %s", r.podAtBaseline)

	r.oldIssuerCA, err = r.cmKube.Get(ctx, "clusterissuer", r.cfg.IssuerName, "{.spec.ca.secretName}")
	if err != nil {
		return fmt.Errorf("read the issuer's CA secret: %w", err)
	}
	if r.oldIssuerCA == "" {
		return fmt.Errorf("ClusterIssuer %s is not a CA issuer (no spec.ca.secretName)", r.cfg.IssuerName)
	}
	return nil
}

// trustNewRoot mints a second root from the bootstrap issuer and appends it to
// the roots ConfigMap, then waits for the bundle to carry it in both namespaces.
func (r *tlsRotationRun) trustNewRoot(ctx context.Context) error {
	f := r.a.f
	r.newRootName = r.cfg.IssuerName + "-rot-" + f.suffix()
	r.newRootCN = "AMP Root CA rotation " + f.suffix()
	manifest := fmt.Sprintf(`apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  isCA: true
  commonName: %[3]q
  secretName: %[1]s
  duration: 87600h
  privateKey:
    algorithm: ECDSA
    size: 256
  issuerRef:
    kind: Issuer
    name: %[4]s
`, r.newRootName, r.cfg.CertManagerNS, r.newRootCN, r.cfg.BootstrapIssuer)
	if err := r.cmKube.Apply(ctx, manifest); err != nil {
		return fmt.Errorf("apply the rotation root: %w", err)
	}
	f.registerLast("delete rotation root "+r.newRootName, func(ctx context.Context) error {
		if err := r.cmKube.Delete(ctx, "certificate", r.newRootName); err != nil {
			return err
		}
		return r.cmKube.Delete(ctx, "secret", r.newRootName)
	})
	if err := r.cmKube.WaitFor(ctx, "certificate/"+r.newRootName, "condition=Ready", 3*time.Minute); err != nil {
		return fmt.Errorf("rotation root never became Ready: %w", err)
	}
	pemB64, err := r.cmKube.Get(ctx, "secret", r.newRootName, `{.data.ca\.crt}`)
	if err != nil {
		return err
	}
	pemBytes, err := base64.StdEncoding.DecodeString(pemB64)
	if err != nil {
		return fmt.Errorf("decode the rotation root: %w", err)
	}
	r.newRootPEM = string(pemBytes)

	r.oldRootsPEM, err = r.cmKube.Get(ctx, "configmap", r.cfg.RootsConfigMap, `{.data.ca\.crt}`)
	if err != nil {
		return fmt.Errorf("read %s: %w", r.cfg.RootsConfigMap, err)
	}
	if err := r.setRoots(ctx, appendRootPEM(r.oldRootsPEM, r.newRootPEM)); err != nil {
		return err
	}
	f.registerLast("restore roots ConfigMap "+r.cfg.RootsConfigMap, func(ctx context.Context) error {
		return r.setRoots(ctx, r.oldRootsPEM)
	})
	for _, k := range []*k8s.Kubectl{r.ampKube, r.ybKube} {
		if err := r.waitBundleHas(ctx, k, r.newRootPEM); err != nil {
			return err
		}
	}
	f.logf("trust: %s carries the rotation root in %s and %s", r.cfg.BundleConfigMap, r.ampKube.Namespace(), r.ybKube.Namespace())
	return nil
}

func (r *tlsRotationRun) repointIssuer(ctx context.Context) error {
	f := r.a.f
	if err := r.setIssuerCA(ctx, r.newRootName); err != nil {
		return err
	}
	f.registerLast("restore ClusterIssuer "+r.cfg.IssuerName, func(ctx context.Context) error {
		return r.setIssuerCA(ctx, r.oldIssuerCA)
	})
	// A repointed CA issuer reports Ready again once it has loaded the Secret.
	return r.cmKube.WaitFor(ctx, "clusterissuer/"+r.cfg.IssuerName, "condition=Ready", 2*time.Minute)
}

// renewAndRoll re-issues every leaf in both namespaces from the new root, then
// restarts the database StatefulSets (masters first) so they run on theirs.
func (r *tlsRotationRun) renewAndRoll(ctx context.Context) error {
	f := r.a.f
	for _, k := range []*k8s.Kubectl{r.ampKube, r.ybKube} {
		certs, err := listCertificates(ctx, k)
		if err != nil {
			return err
		}
		if len(certs) == 0 {
			return fmt.Errorf("no Certificates in %s; the stack is not on cert-manager", k.Namespace())
		}
		before := map[string]string{}
		for name := range certs {
			nb, err := k.Get(ctx, "certificate", name, "{.status.notBefore}")
			if err != nil {
				return err
			}
			before[name] = nb
		}
		// cert-manager re-issues a Certificate whose Secret is gone; the mounted
		// files stay in place until the new Secret lands. This is the renewal
		// trigger that needs no cmctl.
		for name, secret := range certs {
			if err := k.Delete(ctx, "secret", secret); err != nil {
				return fmt.Errorf("delete %s/%s to force a renewal: %w", k.Namespace(), secret, err)
			}
			_ = name
		}
		deadline := time.Now().Add(r.cfg.RenewTimeout)
		for name := range certs {
			for {
				nb, err := k.Get(ctx, "certificate", name, "{.status.notBefore}")
				if err == nil && nb != "" && nb != before[name] {
					break
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("%s/%s was not re-issued within %s (notBefore still %q)", k.Namespace(), name, r.cfg.RenewTimeout, nb)
				}
				time.Sleep(2 * time.Second)
			}
		}
		f.logf("renewed %d certificate(s) in %s", len(certs), k.Namespace())
	}

	// The database trusts the root that signed its leaf and copies the files at
	// pod start, so it moves by restarting; masters before tservers, as the
	// yb-cert-rollout CronJob does.
	for _, sts := range []string{"yb-master", "yb-tserver"} {
		patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`, time.Now().UTC().Format(time.RFC3339))
		if err := r.ybKube.Patch(ctx, "statefulset", sts, "merge", patch); err != nil {
			return fmt.Errorf("restart %s: %w", sts, err)
		}
		if err := r.ybKube.RolloutStatus(ctx, "statefulset", sts, tlsRotationRollout); err != nil {
			return fmt.Errorf("%s did not come back on the new certificates: %w", sts, err)
		}
	}
	return nil
}

// verify: the database serves a new-root leaf, the daemons authenticate with
// new-root leaves without their pod restarting, and the endpoint still works.
func (r *tlsRotationRun) verify(ctx context.Context) error {
	f := r.a.f
	rel, err := ybReleaseName(ctx, r.ybKube)
	if err != nil {
		return fmt.Errorf("find the yugabyte release name: %w", err)
	}
	secret := "yb-tserver-" + rel + "-tls-cert"
	caB64, err := r.ybKube.Get(ctx, "secret", secret, `{.data.ca\.crt}`)
	if err != nil {
		return fmt.Errorf("read %s/%s, the leaf this phase exists to check: %w", r.ybKube.Namespace(), secret, err)
	}
	ca, err := base64.StdEncoding.DecodeString(caB64)
	if err != nil {
		return fmt.Errorf("decode ca.crt out of %s: %w", secret, err)
	}
	if !samePEM(string(ca), r.newRootPEM) {
		return fmt.Errorf("the tserver leaf was not issued by the rotation root")
	}

	deadline := time.Now().Add(tlsRotationVerify)
	for {
		issuers, err := r.daemonIssuers(ctx)
		if err != nil {
			return err
		}
		if strings.Contains(issuers, r.newRootCN) && !strings.Contains(strings.ReplaceAll(issuers, r.newRootCN, ""), "AMP Root CA") {
			f.logf("verify: daemon client certificates now issued by %s", issuers)
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemons still authenticate with the old root after %s: issuers %q", tlsRotationVerify, issuers)
		}
		time.Sleep(3 * time.Second)
	}

	pods, err := r.ampKube.GetSelector(ctx, "pod", "app.kubernetes.io/instance="+endpointDeployName(r.ep.ID), "{range .items[*]}{.status.containerStatuses[0].restartCount}{\" \"}{end}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(pods) != "0" {
		return fmt.Errorf("the endpoint pod restarted during the rotation (restart counts %q); the reload must not need it", pods)
	}

	c, err := f.endpointSQL(ctx, r.ep)
	if err != nil {
		return fmt.Errorf("after the rotation the endpoint no longer accepts connections: %w", err)
	}
	defer c.Close()
	n, err := c.QueryInt(ctx, "SELECT count(*) FROM tls_rotation")
	if err != nil {
		return fmt.Errorf("after the rotation the endpoint no longer answers SQL: %w", err)
	}
	if n != tlsRotationRows {
		return fmt.Errorf("tls_rotation has %d rows after the rotation, want %d", n, tlsRotationRows)
	}
	if _, err := c.Exec(ctx, fmt.Sprintf("INSERT INTO tls_rotation VALUES (%d, 'after')", tlsRotationRows+1)); err != nil {
		return fmt.Errorf("write after the rotation: %w", err)
	}
	return nil
}

// daemonIssuers is the distinct issuer DNs of the certificate-authenticated
// safekeeper/pageserver backends on the backing, '|'-joined; "" when none.
// assertEndpointPodNeverRestarted checks the pod recorded at baseline, by name,
// and its own restart count. Reading `{range .items[*]}` over the instance
// selector concatenates every match — during a reschedule that is "0 0", which
// trips a `!= "0"` comparison; and a *replaced* pod starts at 0, which would
// pass the assertion silently. Pinning the identity is what makes it mean
// anything.
func (r *tlsRotationRun) assertEndpointPodNeverRestarted(ctx context.Context) error {
	if r.podAtBaseline == "" {
		return fmt.Errorf("no endpoint pod was recorded at baseline; cannot tell a restart from a replacement")
	}
	name, err := r.ampKube.Get(ctx, "pod", r.podAtBaseline, "{.metadata.name}")
	if err != nil {
		return fmt.Errorf("endpoint pod %s is gone, so it was replaced during the rotation: %w", r.podAtBaseline, err)
	}
	if strings.TrimSpace(name) != r.podAtBaseline {
		return fmt.Errorf("endpoint pod is now %q, was %q: replaced during the rotation", name, r.podAtBaseline)
	}
	count, err := r.ampKube.Get(ctx, "pod", r.podAtBaseline, "{.status.containerStatuses[0].restartCount}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(count) != "0" {
		return fmt.Errorf("endpoint pod %s restarted during the rotation (restartCount %q); the reload must not need it", r.podAtBaseline, count)
	}
	return nil
}

// connectBacking dials the backing with its tserver address re-resolved. The
// stored ConnInfo holds the pod IP found at provision time, and this rehearsal
// restarts the tserver StatefulSet — so after the roll that IP belongs to a
// deleted pod. It also retries: the SQL port lags pod-Ready, which is why
// ybk8s.waitRolloutAndYSQL pairs its rollout wait with a `select 1`.
func (r *tlsRotationRun) connectBacking(ctx context.Context) (*pg.Client, error) {
	ci := r.a.f.store.ConnInfo()
	deadline := time.Now().Add(tlsRotationVerify)
	var last error
	for {
		if ip, err := r.runningTServerIP(ctx); err == nil && ip != "" {
			ci.Host = ip
		} else if err != nil {
			last = err
		}
		db, err := pg.Connect(ctx, ci)
		if err == nil {
			if _, err = db.QueryString(ctx, "SELECT 1"); err == nil {
				return db, nil
			}
			db.Close()
		}
		last = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("connect to the backing at %s after the roll: %w", ci.Host, last)
		}
		time.Sleep(3 * time.Second)
	}
}

// runningTServerIP is the address of a tserver that is up now, not the one that
// was up when the backing was provisioned.
func (r *tlsRotationRun) runningTServerIP(ctx context.Context) (string, error) {
	out, err := r.ybKube.GetSelector(ctx, "pod", "app=yb-tserver",
		"{.items[?(@.status.phase=='Running')].status.podIP}")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("no running yb-tserver pod")
	}
	return fields[0], nil
}

func (r *tlsRotationRun) daemonIssuers(ctx context.Context) (string, error) {
	db, err := r.connectBacking(ctx)
	if err != nil {
		return "", err
	}
	defer db.Close()
	return db.QueryString(ctx, `SELECT coalesce(string_agg(DISTINCT s.issuer_dn, '|'), '')
		  FROM pg_stat_ssl s JOIN pg_stat_activity a USING (pid)
		 WHERE s.ssl AND s.client_dn IS NOT NULL
		   AND (a.application_name LIKE 'safekeeper:%' OR a.application_name LIKE 'pageserver:%')`)
}

func (r *tlsRotationRun) setRoots(ctx context.Context, pem string) error {
	patch, err := json.Marshal(map[string]any{"data": map[string]string{"ca.crt": pem}})
	if err != nil {
		return err
	}
	if err := r.cmKube.Patch(ctx, "configmap", r.cfg.RootsConfigMap, "merge", string(patch)); err != nil {
		return fmt.Errorf("update %s: %w", r.cfg.RootsConfigMap, err)
	}
	return nil
}

func (r *tlsRotationRun) setIssuerCA(ctx context.Context, secret string) error {
	patch := fmt.Sprintf(`{"spec":{"ca":{"secretName":%q}}}`, secret)
	if err := r.cmKube.Patch(ctx, "clusterissuer", r.cfg.IssuerName, "merge", patch); err != nil {
		return fmt.Errorf("repoint ClusterIssuer %s at %s: %w", r.cfg.IssuerName, secret, err)
	}
	return nil
}

func (r *tlsRotationRun) waitBundleHas(ctx context.Context, k *k8s.Kubectl, pem string) error {
	deadline := time.Now().Add(tlsRotationBundleUp)
	for {
		got, err := k.Get(ctx, "configmap", r.cfg.BundleConfigMap, `{.data.ca\.crt}`)
		if err == nil && containsPEM(got, pem) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s in %s did not pick up the rotation root within %s", r.cfg.BundleConfigMap, k.Namespace(), tlsRotationBundleUp)
		}
		time.Sleep(3 * time.Second)
	}
}

// listCertificates maps Certificate name → secretName for a namespace.
func listCertificates(ctx context.Context, k *k8s.Kubectl) (map[string]string, error) {
	out, err := k.GetSelector(ctx, "certificates.cert-manager.io", "", `{range .items[*]}{.metadata.name}{"="}{.spec.secretName}{"\n"}{end}`)
	if err != nil {
		return nil, fmt.Errorf("list certificates in %s: %w", k.Namespace(), err)
	}
	certs := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		name, secret, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && name != "" && secret != "" {
			certs[name] = secret
		}
	}
	return certs, nil
}

// ybReleaseName is the chart release the yb-tserver StatefulSet belongs to
// (its `release` label), which names the chart's Certificates and the
// tserver Secret is named from it. No literal fallback: guessing "yugabyte"
// turns a renamed release into a silently skipped assertion.
func ybReleaseName(ctx context.Context, k *k8s.Kubectl) (string, error) {
	rel, err := k.Get(ctx, "statefulset", "yb-tserver", "{.metadata.labels.release}")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(rel) == "" {
		return "", fmt.Errorf("statefulset yb-tserver in %s carries no release label", k.Namespace())
	}
	return strings.TrimSpace(rel), nil
}

// appendRootPEM adds root to a PEM bundle unless it is already there.
func appendRootPEM(bundle, root string) string {
	if containsPEM(bundle, root) {
		return bundle
	}
	return strings.TrimRight(bundle, "\n") + "\n" + strings.TrimSpace(root) + "\n"
}

// containsPEM reports whether bundle carries the certificate in pem, comparing
// the base64 body so whitespace differences do not matter.
func containsPEM(bundle, pem string) bool {
	body := pemBody(pem)
	return body != "" && strings.Contains(strings.Join(strings.Fields(bundle), ""), body)
}

// samePEM reports whether two PEM texts hold the same single certificate.
func samePEM(a, b string) bool {
	return pemBody(a) != "" && pemBody(a) == pemBody(b)
}

func pemBody(pem string) string {
	s := strings.Join(strings.Fields(pem), "")
	s = strings.TrimPrefix(s, "-----BEGINCERTIFICATE-----")
	if i := strings.Index(s, "-----END"); i >= 0 {
		s = s[:i]
	}
	return s
}
