package foundation

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// DownOptions controls how much a teardown removes.
type DownOptions struct {
	// DeleteNamespace removes the namespace too. OFF by default, and that is a
	// safety property rather than a default worth flipping: this namespace is
	// SHARED with the consumer's workloads — the amp controller and its
	// endpoints run in it — so deleting it would take them down with the
	// foundation. Pass it only for a namespace nothing else lives in, such as a
	// per-run CI namespace being reaped anyway.
	DeleteNamespace bool
	// DeleteCRDs removes the operator's CustomResourceDefinitions. OFF by
	// default because they are cluster-scoped and SHARED: deleting them deletes
	// every other foundation's universe on the cluster along with this one.
	DeleteCRDs bool
	// FinalizerTimeout bounds the wait for the CR's finalizer. The operator
	// deletes the universe before releasing it, so this is the real teardown.
	FinalizerTimeout time.Duration
}

// DefaultFinalizerTimeout is how long to wait for the operator to delete the
// universe and release the CR.
const DefaultFinalizerTimeout = 30 * time.Minute

// Down tears the foundation down in dependency order: the universe first (so
// the operator deletes the DB before its own RBAC disappears), then the helm
// release, then the cluster-scoped RBAC the chart leaves behind.
//
// Every step runs even if an earlier one failed, and the errors are collected:
// a teardown that stops at the first failure leaks whatever came after it, and
// the cluster-scoped objects at the end are exactly what nothing else reclaims.
//
// It does NOT stop the consumer's workloads, and with the namespace left in
// place (the default) they outlive the database they were pointed at: the
// controller and its endpoints carry the Service name in AMP_DATABASE_URL and
// AMP_DEPLOY_YB_HOST_PORTS, and Kubernetes does not rewrite env in a live
// container, so their next reconnect resolves a name that is gone. That is the
// crash-loop amp/runbooks/controller_runbook.md describes under "DB URL must
// match how YugabyteDB was stood up". Inherent to deleting a foundation out
// from under its consumers rather than a defect, but scale them down first if
// you want a quiet teardown.
func (d *Deployer) Down(ctx context.Context, opts DownOptions) error {
	if opts.FinalizerTimeout == 0 {
		opts.FinalizerTimeout = DefaultFinalizerTimeout
	}
	var errs []string
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	// The tserver Service is NOT deleted here. It is a Namespaced-scope
	// serviceEndpoint, which the chart deliberately does not own: it is shared
	// across releases (only the owning release re-renders it —
	// `yugabyte.should_render_service`) and carries helm.sh/resource-policy:
	// keep so a non-owning release's uninstall cannot take it from the others.
	// Its lifecycle is tied to the UNIVERSE's, and YBA deletes it when the
	// universe goes — which is what deleting the CR below triggers. Deleting it
	// here would race that and could remove a Service another universe in this
	// namespace still depends on.
	//
	// The foundation CI asserts it is gone afterwards, so if YBA turns out not
	// to reclaim it, the run says so rather than leaking silently.
	d.log("deleting YBUniverse %s", d.spec.UniverseName())
	if err := d.deleteCR(ctx, "ybuniverse", d.spec.UniverseName(), opts.FinalizerTimeout); err != nil {
		fail("%v", err)
	}

	// The provider goes after the universe (which references it) and before the
	// helm uninstall, for the same finalizer reason: only the operator can clear
	// it, so removing the operator first strands it — and a stranded finalizer
	// on a namespaced object is what leaves a namespace Terminating forever.
	//
	// Only one we created. A spec that just NAMES a provider shares it with
	// every other universe on the cluster, and deleting that would take them
	// with it.
	if len(d.spec.Provider) > 0 {
		name := d.spec.ProviderName()
		d.log("deleting YBProvider %s", name)
		if err := d.deleteCR(ctx, "ybprovider", name, opts.FinalizerTimeout); err != nil {
			fail("%v", err)
		}
	}

	release := d.spec.HelmRelease()
	d.log("uninstalling helm release %s", release)
	if res, err := d.helm(ctx, "uninstall", release, "--namespace", d.spec.Namespace, "--ignore-not-found"); err != nil {
		fail("helm uninstall %s: %v", release, err)
	} else if res.ExitCode != 0 {
		fail("helm uninstall %s: %s", release, strings.TrimSpace(res.Stderr))
	}

	// The chart's ClusterRole and ClusterRoleBinding are named after the helm
	// release, and a namespace delete never reclaims a cluster-scoped object.
	// Left behind they collide with the next foundation that uses this release
	// name — the leak ybastack documents. Safe to name directly because the
	// release name carries our namespace.
	for _, kind := range []string{"clusterrolebinding", "clusterrole"} {
		if err := d.deleteClusterScoped(ctx, kind, release); err != nil {
			fail("%v", err)
		}
	}

	if opts.DeleteNamespace {
		if err := d.deleteNamespace(ctx); err != nil {
			fail("%v", err)
		}
	} else {
		d.log("leaving namespace %s in place (it is shared with the consumer's workloads; "+
			"pass --delete-namespace to remove it)", d.spec.Namespace)
	}

	if opts.DeleteCRDs {
		d.log("deleting operator CRDs (cluster-scoped and shared)")
		if err := d.deleteByArgs(ctx, "CRDs", "delete", "-f", d.crdSource(), "--ignore-not-found"); err != nil {
			fail("%v", err)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("foundation down: %s", strings.Join(errs, "; "))
	}
	return nil
}

// deleteCR deletes one of the operator's CRs and waits for its finalizer to
// clear.
//
// The wait is the point, and it is why this is not just a kubectl delete: the
// operator does the real work (deleting the universe in YBA, deregistering the
// provider) BEFORE it releases the CR, so returning as soon as kubectl does
// would leave the DB pods and their PVCs running — and the helm uninstall that
// follows would then remove the operator mid-delete.
//
// --wait=false plus an explicit poll rather than a blocking delete, so the
// timeout is ours and its message can say what a stuck finalizer means.
func (d *Deployer) deleteCR(ctx context.Context, kind, name string, timeout time.Duration) error {
	res, err := d.kubectl(ctx, nil, "delete", kind, name, "--ignore-not-found", "--wait=false")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("delete %s %s: %s", kind, name, strings.TrimSpace(res.Stderr))
	}

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()
	for {
		present, err := d.crPresent(ctx, kind, name)
		if err != nil {
			return err
		}
		if !present {
			d.log("%s %s is gone", kind, name)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s %s still present after %s; its finalizer has not cleared "+
				"(the operator may be unable to delete it — check its logs before removing the "+
				"finalizer by hand, which orphans whatever it was still tearing down)",
				kind, name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// crPresent reports whether a CR still exists. A NotFound is the answer rather
// than an error — it is what the caller is waiting for. Anything else (no such
// CRD, no permission, unreachable cluster) is a real failure, because reading it
// as absence would report a leaked CR as reclaimed.
func (d *Deployer) crPresent(ctx context.Context, kind, name string) (bool, error) {
	res, err := d.kubectl(ctx, nil, "get", kind, name, "-o", "name")
	if err != nil {
		return false, err
	}
	if res.ExitCode == 0 {
		return true, nil
	}
	if strings.Contains(res.Stderr, "NotFound") || strings.Contains(res.Stderr, "not found") {
		return false, nil
	}
	return false, fmt.Errorf("get %s %s: %s", kind, name, strings.TrimSpace(res.Stderr))
}

// deleteNamespace removes the namespace, but only one this package created.
//
// The guard matters because the default namespace holds the consumer's
// workloads: without it, a Down against a hand-made or shared namespace would
// delete the amp controller, its endpoints and anything else living there.
func (d *Deployer) deleteNamespace(ctx context.Context) error {
	res, err := d.kubectlCluster(ctx, nil, "get", "namespace", d.spec.Namespace,
		"-o", "jsonpath={.metadata.labels.app\\.kubernetes\\.io/managed-by}")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return nil // already gone
	}
	if got := strings.TrimSpace(res.Stdout); got != "meko-foundation" {
		return fmt.Errorf("refusing to delete namespace %s: it is not foundation-managed "+
			"(app.kubernetes.io/managed-by=%q), so it may hold workloads this deployer did not create",
			d.spec.Namespace, got)
	}
	d.log("deleting namespace %s", d.spec.Namespace)
	return d.deleteByArgs(ctx, "namespace "+d.spec.Namespace,
		"delete", "namespace", d.spec.Namespace, "--ignore-not-found")
}

// deleteClusterScoped deletes one cluster-scoped object by kind and name.
func (d *Deployer) deleteClusterScoped(ctx context.Context, kind, name string) error {
	return d.deleteByArgs(ctx, kind+" "+name, "delete", kind, name, "--ignore-not-found")
}

// deleteByArgs runs a kubectl delete and treats a NON-ZERO EXIT as a failure.
//
// That is the whole point of it existing. Exec deliberately returns a non-zero
// exit as data rather than an error ("absent" and "broken" are both non-zero and
// only the caller knows which it asked about), so a delete that only checks the
// error reports success for a permission denial or an invalid object — and in a
// teardown that means silently leaking exactly the cluster-scoped objects
// nothing else reclaims. --ignore-not-found already makes absence a zero exit,
// so anything non-zero here is real.
func (d *Deployer) deleteByArgs(ctx context.Context, what string, args ...string) error {
	res, err := d.kubectlCluster(ctx, nil, args...)
	if err != nil {
		return fmt.Errorf("delete %s: %w", what, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("delete %s: %s", what, strings.TrimSpace(res.Stderr))
	}
	return nil
}
