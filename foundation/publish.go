package foundation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ConnConfigMap is where consumers read the address from.
const ConnConfigMap = "foundation-conn"

// PublishConn writes the address into a ConfigMap in namespace, creating or
// replacing it.
//
// A ConfigMap and not the status, because a CR's status cannot be wired into a
// pod: Kubernetes has configMapKeyRef and no customResourceKeyRef, so this is
// the only form a consumer Deployment can consume —
//
//	env:
//	  - name: YB_HOST_PORTS
//	    valueFrom:
//	      configMapKeyRef: {name: foundation-conn, key: YB_HOST_PORTS}
//
// and the amp controller takes its address as environment variables.
//
// It publishes the PARTS rather than a finished DSN. Deployer.dsn appends
// sslrootcert/sslcert/sslkey from options the consumer supplies, and those paths
// are facts about the consumer's filesystem — up-gke.sh passes /etc/amp/tls/…
// paths that exist only in amp's pods — so one baked DSN cannot be right for
// everybody.
//
// Not a Secret: the DSN carries no password, the user is `yugabyte`, and the
// Service name is not a secret either.
func (d *Deployer) PublishConn(ctx context.Context, namespace string, info *Info) error {
	if info == nil {
		return fmt.Errorf("no connection details to publish; the universe has no address yet")
	}
	data := map[string]string{
		"YB_HOST_PORTS": info.YSQLHostPorts,
		"YB_SSLMODE":    d.SSLMode(),
	}
	if info.UniverseNamespace != "" {
		data["YB_UNIVERSE_NAMESPACE"] = info.UniverseNamespace
	}
	cm := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      ConnConfigMap,
			"namespace": namespace,
			"labels":    map[string]any{ManagedByLabel: ManagedByValue},
		},
		"data": data,
	}
	body, err := json.Marshal(cm)
	if err != nil {
		return err
	}
	// Server-side apply with force, like every other object this writes: a
	// consumer that once edited the ConfigMap by hand would otherwise be a
	// field-manager conflict on every converge, and the foundation is the
	// source of truth for the address.
	res, err := d.kubectlIn(ctx, namespace, body, "apply", "--server-side", "--force-conflicts", "-f", "-")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("publish %s/%s: %s", namespace, ConnConfigMap, strings.TrimSpace(res.Stderr))
	}
	d.log("published the address to configmap/%s in %s", ConnConfigMap, namespace)
	return nil
}

// unpublishConn removes what PublishConn wrote in the foundation's own
// namespace.
//
// Nothing else reclaims it. The labelled sweep `apply` and `down` share asks
// only for the operator's API group, so a ConfigMap in the core group is never
// a candidate however it is labelled — which is also why a converge does not
// delete it between publishes. A consumer left reading it after a teardown
// would dial a Service that no longer exists.
//
// It cannot clean up after `converge --publish-to <ns>`, which writes wherever
// it was told; that one is the caller's.
func (d *Deployer) unpublishConn(ctx context.Context) error {
	res, err := d.kubectlIn(ctx, d.spec.Namespace, nil,
		"delete", "configmap", ConnConfigMap, "--ignore-not-found")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("delete %s/%s: %s", d.spec.Namespace, ConnConfigMap, strings.TrimSpace(res.Stderr))
	}
	return nil
}
