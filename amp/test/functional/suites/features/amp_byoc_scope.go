// amp_byoc_scope proves the controller runs on namespace-scoped RBAC only, the
// permission shape a BYOC install grants. The verdict comes from the API server
// (kubectl auth can-i --as the controller's service account), not from the
// rendered chart: a chart can look namespace-scoped while a stray
// ClusterRoleBinding from elsewhere still holds the service account
// cluster-scoped rights.
package features

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
)

const (
	byocKubectlBin = "kubectl"
	byocRows       = 25
)

// errByocNoImpersonation is the honest fallback trigger: asking the API server
// what another subject may do needs impersonate rights of our own.
var errByocNoImpersonation = errors.New("kubectl auth can-i --as is not permitted for the credentials this run holds")

// byocDeniedProbes are the cluster-scoped actions a namespace-scoped controller
// must not be able to take. Each is asked with --all-namespaces so a namespaced
// Role can never answer for it.
var byocDeniedProbes = []rbacProbe{
	{verb: "list", resource: "namespaces", cluster: true},
	{verb: "list", resource: "nodes", cluster: true},
	{verb: "list", resource: "pods", cluster: true},
	{verb: "get", resource: "clusterroles", cluster: true},
}

// byocAllowedProbes are the namespaced actions the chart's controller Role
// grants and the controller genuinely takes (it drives Helm for endpoint
// workloads and reads pod/service addresses). A controller denied these is
// broken in the opposite direction.
var byocAllowedProbes = []rbacProbe{
	{verb: "create", resource: "pods"},
	{verb: "get", resource: "services"},
	{verb: "create", resource: "configmaps"},
	{verb: "get", resource: "secrets"},
	{verb: "create", resource: "deployments"},
	{verb: "create", resource: "statefulsets"},
}

func init() {
	core.Register(core.Test{
		Name:        "features/amp_byoc_scope",
		Suite:       "features",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SharedControlPlane},
		Tags:        []string{"regression"},
		OwningTeam:  core.TeamControlPlane,
		Description: "Prove the controller operates under namespace-scoped RBAC only, as a BYOC install grants: the API server denies the controller's service account every cluster-scoped probe, still grants the namespaced ones its Role declares, and a real endpoint deploy plus query goes through under that restriction.",
		Params:      []string{"functional", "env.ampstack", "apps.ybastack", "apps.yba", "apps.ybk8s"},
		Timeout:     20 * time.Minute,
		Run:         runAmpByocScope,
	})
}

func runAmpByocScope(t *core.T) error {
	requireImpersonation := t.Options.GetBool("functional.byoc.require_impersonation", false)
	assertNoPublicLB := t.Options.GetBool("functional.byoc.assert_no_public_lb", false)
	kubeContext := byocKubeContext(t.Options)
	return runAmpFeature(t, func(a *ampFeatureRun) []phase {
		b := &byocRun{
			a:                    a,
			kubeContext:          kubeContext,
			requireImpersonation: requireImpersonation,
			assertNoPublicLB:     assertNoPublicLB,
		}
		return []phase{
			{"permission_scope", b.permissionScope},
			{"namespace_scoped_end_to_end", b.endToEnd},
			{"loadbalancer_services", b.loadBalancerServices},
		}
	})
}

// byocKubeContext resolves the kube-context of the cluster the control plane runs
// on, the same order sharedk8s resolves it in.
func byocKubeContext(c *core.Config) string {
	if v := c.GetString("env.sharedk8s.context", ""); v != "" {
		return v
	}
	return ampstack.FromConfig(c).Context
}

type byocRun struct {
	a                    *ampFeatureRun
	kubeContext          string
	requireImpersonation bool
	assertNoPublicLB     bool
}

// rbacProbe is one question about what the controller's service account may do.
type rbacProbe struct {
	verb     string
	resource string
	cluster  bool
}

func (p rbacProbe) String() string {
	if p.cluster {
		return p.verb + " " + p.resource + " at cluster scope"
	}
	return p.verb + " " + p.resource + " in the controller namespace"
}

// canIFunc answers one probe with the API server's own verdict, "yes" or "no".
type canIFunc func(ctx context.Context, p rbacProbe) (string, error)

func (b *byocRun) record(name string, props map[string]any) {
	_ = b.a.f.t.Step(name, func(h *core.StepHandle) error {
		for k, v := range props {
			h.Set(k, v)
		}
		return nil
	})
}

func (b *byocRun) permissionScope(ctx context.Context) error {
	kube := b.a.f.ampKube()
	if kube == nil {
		return core.AsEnvFailure(fmt.Errorf("no kubectl for the control-plane namespace (direct-URL control plane), so the controller's permissions cannot be probed"))
	}
	ns := kube.Namespace()
	if ns == "" {
		return core.AsEnvFailure(fmt.Errorf("the control-plane kubectl carries no namespace, so the controller's permissions cannot be probed"))
	}
	sa, err := byocControllerServiceAccount(ctx, kube)
	if err != nil {
		return err
	}
	probe := &byocKube{run: kube.Runner(), bin: byocKubectlBin, kubeContext: b.kubeContext}
	canI := func(ctx context.Context, p rbacProbe) (string, error) { return probe.canI(ctx, p, ns, sa) }

	answers, err := byocPermissionSteps(ctx, canI)
	b.record("permission_probes", map[string]any{
		"service_account": ns + "/" + sa,
		"answers":         byocAnswerLine(answers),
		"decided_by":      "api server (kubectl auth can-i --as)",
	})
	if !errors.Is(err, errByocNoImpersonation) {
		return err
	}
	if b.requireImpersonation {
		return fmt.Errorf("the cluster-scope denials could not be asked of the API server and functional.byoc.require_impersonation demands they are: %w", err)
	}
	b.a.f.logf("byoc_scope: %v; falling back to the binding-level check, which is weaker", err)
	bindings, berr := byocBindingSteps(ctx, probe.bindings, ns, sa)
	b.record("permission_bindings", map[string]any{
		"service_account": ns + "/" + sa,
		"bindings":        strings.Join(bindings, " "),
		"decided_by":      "binding enumeration (impersonation unavailable)",
	})
	return berr
}

// byocPermissionSteps asks the API server both halves and returns every answer it
// gave: the cluster-scoped probes must come back "no", the namespaced ones "yes".
// Checking only the denials would also pass for a controller holding no
// permissions at all.
func byocPermissionSteps(ctx context.Context, canI canIFunc) (map[string]string, error) {
	answers := map[string]string{}
	for _, p := range byocDeniedProbes {
		ans, err := canI(ctx, p)
		if err != nil {
			return answers, err
		}
		answers[p.String()] = ans
		if ans != "no" {
			return answers, fmt.Errorf("probe %q: the API server answered %q, want \"no\": the controller's service account still holds a cluster-scoped permission, which a BYOC install does not grant, so something outside the chart's namespace-scoped RBAC is binding it", p, ans)
		}
	}
	for _, p := range byocAllowedProbes {
		ans, err := canI(ctx, p)
		if err != nil {
			return answers, err
		}
		answers[p.String()] = ans
		if ans != "yes" {
			return answers, fmt.Errorf("probe %q: the API server answered %q, want \"yes\": the controller chart's Role declares this verb and the controller uses it, so a namespace-scoped install that lacks it cannot deploy an endpoint", p, ans)
		}
	}
	return answers, nil
}

// byocBindingSteps is the honest fallback when impersonation is unavailable: no
// ClusterRoleBinding may reference the controller's service account, and at least
// one RoleBinding in its namespace must. It returns the bindings it saw.
func byocBindingSteps(ctx context.Context, list byocBindingLister, ns, sa string) ([]string, error) {
	subject := "ServiceAccount/" + ns + "/" + sa
	seen := []string{}
	crbs, err := list(ctx, true, "")
	if err != nil {
		return seen, err
	}
	for _, bnd := range crbs {
		if bnd.references(subject) {
			seen = append(seen, "ClusterRoleBinding/"+bnd.name)
			return seen, fmt.Errorf("ClusterRoleBinding %s binds %s to %s: the controller's service account holds cluster-scoped rights, which a BYOC install does not grant. This is a binding-level check, not the API server's own decision: kubectl auth can-i --as is not permitted for this run's credentials",
				bnd.name, bnd.roleRef, subject)
		}
	}
	rbs, err := list(ctx, false, ns)
	if err != nil {
		return seen, err
	}
	for _, bnd := range rbs {
		if bnd.references(subject) {
			seen = append(seen, "RoleBinding/"+bnd.name)
		}
	}
	if len(seen) == 0 {
		return seen, fmt.Errorf("no RoleBinding in namespace %s binds %s: the controller would hold no namespaced permissions at all, and it needs them to deploy an endpoint. This is a binding-level check, not the API server's own decision: kubectl auth can-i --as is not permitted for this run's credentials", ns, subject)
	}
	return seen, nil
}

// endToEnd drives a real endpoint + query through the controller under the
// restriction the phase above proved. Endpoint deploy sizes the compute pod's
// emptyDir from the node list, a cluster-scoped read the controller does not have
// here (meko#584), so this is where a re-introduced dependency surfaces.
func (b *byocRun) endToEnd(ctx context.Context) error {
	cl, err := b.a.f.controllerClient(ctx)
	if err != nil {
		return err
	}
	// The endpoint deploy is the cluster-scoped dependency (#584). A fork adds none
	// and cannot be taken here anyway: the parent carries no endpoint, so the
	// controller has no fork point to resolve.
	ep, err := createEndpoint(ctx, cl, b.a.proj.poolID, b.a.proj.projID, b.a.proj.branchID)
	if err != nil {
		return fmt.Errorf("create endpoint on a namespace-scoped controller: %w", err)
	}
	c, err := b.a.f.endpointSQL(ctx, ep)
	if err != nil {
		return fmt.Errorf("connect the endpoint a namespace-scoped controller deployed: %w", err)
	}
	defer c.Close()
	table := "byoc_" + b.a.f.suffix()
	if _, err := c.Exec(ctx, "CREATE TABLE "+table+" (id int primary key)"); err != nil {
		return fmt.Errorf("create table through the endpoint: %w", err)
	}
	if _, err := c.Exec(ctx, "INSERT INTO "+table+" (id) SELECT generate_series(1, $1)", byocRows); err != nil {
		return fmt.Errorf("seed rows through the endpoint: %w", err)
	}
	n, err := c.QueryInt(ctx, "SELECT count(*) FROM "+table)
	if err != nil {
		return fmt.Errorf("read rows back through the endpoint: %w", err)
	}
	if n != byocRows {
		return fmt.Errorf("table %s holds %d rows after seeding %d through an endpoint a namespace-scoped controller deployed", table, n, byocRows)
	}
	return nil
}

// loadBalancerServices records how many LoadBalancer Services this run's own
// namespaces carry (meko#709: the ybk8s backing creates three public ones). The
// assertion is opt-in because the framework's backing creates them by design, so
// it fails by construction today.
func (b *byocRun) loadBalancerServices(ctx context.Context) error {
	kube := b.a.f.ampKube()
	if kube == nil {
		return nil
	}
	probe := &byocKube{run: kube.Runner(), bin: byocKubectlBin, kubeContext: b.kubeContext}
	var found []string
	for _, ns := range b.runNamespaces(kube.Namespace()) {
		svcs, err := probe.loadBalancerServices(ctx, ns)
		if err != nil {
			b.a.f.logf("byoc_scope: could not list Services in namespace %s (best-effort): %v", ns, err)
			continue
		}
		for _, s := range svcs {
			found = append(found, ns+"/"+s)
		}
	}
	b.record("loadbalancer_services", map[string]any{
		"count":    len(found),
		"services": strings.Join(found, " "),
		"asserted": b.assertNoPublicLB,
	})
	if b.assertNoPublicLB && len(found) > 0 {
		return fmt.Errorf("this run's namespaces carry %d LoadBalancer Service(s) [%s], want none: functional.byoc.assert_no_public_lb is on, and a BYOC install should expose no public address per ephemeral backing",
			len(found), strings.Join(found, " "))
	}
	return nil
}

// runNamespaces are the namespaces this run created: the amp control plane's plus
// whatever the backing provisioner recorded for reaping. Nothing else is ours to
// look at.
func (b *byocRun) runNamespaces(ampNS string) []string {
	out := []string{}
	seen := map[string]bool{}
	add := func(ns string) {
		if ns == "" || seen[ns] {
			return
		}
		seen[ns] = true
		out = append(out, ns)
	}
	add(ampNS)
	if b.a.f.reap != nil {
		for _, n := range b.a.f.reap.Namespaces {
			add(n.Namespace)
		}
	}
	return out
}

// byocControllerServiceAccount reads the service account the controller
// Deployment actually runs as, rather than assuming the chart's name.
func byocControllerServiceAccount(ctx context.Context, kube *k8s.Kubectl) (string, error) {
	sa, err := kube.Get(ctx, "deployment", ampstack.DefaultControllerName, "{.spec.template.spec.serviceAccountName}")
	if err != nil {
		return "", fmt.Errorf("read the service account of deployment %s in namespace %s: %w", ampstack.DefaultControllerName, kube.Namespace(), err)
	}
	if strings.TrimSpace(sa) == "" {
		return "", fmt.Errorf("deployment %s in namespace %s names no service account, so there is no subject to probe", ampstack.DefaultControllerName, kube.Namespace())
	}
	return strings.TrimSpace(sa), nil
}

// byocKube runs the kubectl subcommands this test needs and the k8s.Kubectl
// helpers do not cover. `auth can-i` exits 1 for a plain "no", which those
// helpers report as a command failure, so the runner is driven directly.
type byocKube struct {
	run         k8s.Runner
	bin         string
	kubeContext string
}

func (p *byocKube) exec(ctx context.Context, extra ...string) (exec.Result, error) {
	argv := []string{p.bin}
	if p.kubeContext != "" {
		argv = append(argv, "--context", p.kubeContext)
	}
	return p.run(ctx, nil, append(argv, extra...))
}

// canI asks the API server whether the service account may take one action.
func (p *byocKube) canI(ctx context.Context, probe rbacProbe, ns, sa string) (string, error) {
	// --all-namespaces is a flag of `auth can-i`, not a kubectl global, so the
	// scope flags must follow the subcommand: kubectl parses only its own globals
	// before a command and treats the rest as a plugin invocation.
	extra := []string{"auth", "can-i", probe.verb, probe.resource,
		"--as", "system:serviceaccount:" + ns + ":" + sa}
	if probe.cluster {
		extra = append(extra, "--all-namespaces")
	} else {
		extra = append(extra, "-n", ns)
	}
	res, err := p.exec(ctx, extra...)
	if err != nil {
		return "", fmt.Errorf("run kubectl auth can-i %s: %w", probe, err)
	}
	switch ans := byocLastLine(res.Stdout); ans {
	case "yes", "no":
		return ans, nil
	}
	if strings.Contains(res.Stderr, "impersonate") {
		return "", fmt.Errorf("probe %q: %w: %s", probe, errByocNoImpersonation, strings.TrimSpace(res.Stderr))
	}
	return "", fmt.Errorf("probe %q: kubectl auth can-i answered neither yes nor no (exit %d): %s",
		probe, res.ExitCode, strings.TrimSpace(res.Stderr+" "+res.Stdout))
}

// byocBinding is one (Cluster)RoleBinding: what it grants and to whom.
type byocBinding struct {
	name     string
	roleRef  string
	subjects []string
}

func (b byocBinding) references(subject string) bool {
	for _, s := range b.subjects {
		if s == subject {
			return true
		}
	}
	return false
}

// byocBindingLister lists cluster-scoped or namespaced role bindings.
type byocBindingLister func(ctx context.Context, cluster bool, ns string) ([]byocBinding, error)

const byocBindingJSONPath = `{range .items[*]}{.metadata.name}{"|"}{.roleRef.kind}/{.roleRef.name}{"|"}` +
	`{range .subjects[*]}{.kind}/{.namespace}/{.name}{" "}{end}{"\n"}{end}`

func (p *byocKube) bindings(ctx context.Context, cluster bool, ns string) ([]byocBinding, error) {
	kind, scope := "rolebindings", []string{"-n", ns}
	if cluster {
		kind, scope = "clusterrolebindings", nil
	}
	extra := append(scope, "get", kind, "-o", "jsonpath="+byocBindingJSONPath)
	res, err := p.exec(ctx, extra...)
	if err != nil || res.ExitCode != 0 {
		return nil, fmt.Errorf("list %s: exit %d: %v %s", kind, res.ExitCode, err, strings.TrimSpace(res.Stderr))
	}
	return byocParseBindings(res.Stdout), nil
}

func byocParseBindings(out string) []byocBinding {
	var bindings []byocBinding
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(strings.TrimSpace(line), "|")
		if len(fields) < 3 || fields[0] == "" {
			continue
		}
		bindings = append(bindings, byocBinding{
			name:     fields[0],
			roleRef:  fields[1],
			subjects: strings.Fields(fields[2]),
		})
	}
	return bindings
}

// loadBalancerServices returns the names of the namespace's LoadBalancer Services.
func (p *byocKube) loadBalancerServices(ctx context.Context, ns string) ([]string, error) {
	const jsonpath = `{range .items[*]}{.metadata.name}{"|"}{.spec.type}{"\n"}{end}`
	res, err := p.exec(ctx, "-n", ns, "get", "services", "-o", "jsonpath="+jsonpath)
	if err != nil || res.ExitCode != 0 {
		return nil, fmt.Errorf("list services: exit %d: %v %s", res.ExitCode, err, strings.TrimSpace(res.Stderr))
	}
	return byocParseLoadBalancers(res.Stdout), nil
}

func byocParseLoadBalancers(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		name, typ, ok := strings.Cut(strings.TrimSpace(line), "|")
		if !ok || name == "" {
			continue
		}
		if strings.EqualFold(typ, "LoadBalancer") {
			names = append(names, name)
		}
	}
	return names
}

// byocLastLine is the answer kubectl printed, ignoring any leading noise.
func byocLastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// byocAnswerLine renders the probe answers in probe order for the step property.
func byocAnswerLine(answers map[string]string) string {
	var parts []string
	for _, p := range append(append([]rbacProbe{}, byocDeniedProbes...), byocAllowedProbes...) {
		if ans, ok := answers[p.String()]; ok {
			parts = append(parts, p.String()+"="+ans)
		}
	}
	return strings.Join(parts, "; ")
}
