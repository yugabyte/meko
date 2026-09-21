package foundation

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Info is how consumers reach the foundation. It is what `connstr` emits and
// what up-gke.sh evaluates.
type Info struct {
	Namespace       string `json:"namespace"`
	UniverseName    string `json:"universe_name"`
	State           string `json:"state"`
	SoftwareVersion string `json:"software_version,omitempty"`
	NumNodes        int    `json:"num_nodes"`
	// YSQLHostPorts is YB_HOST_PORTS, and it is the ONLY address the deploy path
	// hands compute. Both lanes come off it — see resolveYSQLHost.
	YSQLHostPorts string `json:"ysql_host_ports"`
	// RPCHostPorts is where the direct-RPC lane lands, for diagnostics only. It
	// is not exported and nothing configures it: the lane derives this itself
	// from YSQLHostPorts' host with the RPC port appended. Reported so a reader
	// can see which address must have 9100 open, and so CI can assert it.
	RPCHostPorts string `json:"rpc_host_ports"`
	// DBURL is a DSN for the first requested database, which is what the amp
	// controller takes as DB_URL.
	DBURL string `json:"db_url,omitempty"`
	// SQLEndpoints is what the CR reports in status.sqlEndpoints, carried for
	// diagnostics only. It is deliberately NOT used as YSQLHostPorts: it may
	// enumerate per-pod endpoints, and a per-pod list is exactly what a full
	// move invalidates.
	SQLEndpoints []string `json:"sql_endpoints,omitempty"`
}

// ShellExports renders Info as shell assignments for `eval`, which is how the
// deploy scripts consume it.
//
// One address and one DSN, deliberately: there is no RPC export because nothing
// could consume one. Emitting a variable no consumer reads is worse than
// emitting nothing, because it reads as wired.
func (i *Info) ShellExports() string {
	var b strings.Builder
	fmt.Fprintf(&b, "export YB_HOST_PORTS=%q\n", i.YSQLHostPorts)
	if i.DBURL != "" {
		fmt.Fprintf(&b, "export DB_URL=%q\n", i.DBURL)
	}
	return b.String()
}

// Connect resolves how to reach the foundation.
func (d *Deployer) Connect(ctx context.Context) (*Info, error) {
	status, present, err := d.getUniverseStatus(ctx)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, fmt.Errorf("universe %s does not exist in namespace %s; run `foundation up` first",
			d.spec.UniverseName(), d.spec.Namespace)
	}
	host, err := d.resolveYSQLHost(ctx)
	if err != nil {
		return nil, err
	}
	info := &Info{
		Namespace:       d.spec.Namespace,
		UniverseName:    d.spec.UniverseName(),
		State:           status.UniverseState,
		SoftwareVersion: universeString(d.spec.universe(), "ybSoftwareVersion"),
		NumNodes:        universeInt(d.spec.universe(), "numNodes"),
		// One host, both lanes: YSQL on 5433 as given, and the direct-RPC lane
		// derives 9100 from the same host.
		YSQLHostPorts: fmt.Sprintf("%s:%d", host, DefaultYSQLPort),
		RPCHostPorts:  fmt.Sprintf("%s:%d", host, TServerRPCPort),
		SQLEndpoints:  status.SQLEndpoints,
	}
	if len(d.spec.Databases) > 0 {
		info.DBURL = fmt.Sprintf("postgres://yugabyte@%s/%s?sslmode=%s",
			info.YSQLHostPorts, d.spec.Databases[0], d.sslMode())
	}
	return info, nil
}

// resolveYSQLHost is the address consumers are handed.
//
// It is the chart-created ClusterIP Service carrying both tserver client ports,
// found by label rather than named — YBA prefixes a Namespaced-scope endpoint
// with the universe name it generated, suffix included (tserverServiceName).
//
// ONE address serves both compute lanes, because the direct-RPC lane cannot be
// addressed separately. The deploy path has no way to set tserver_rpc_addrs —
// endpoint_supervisor writes only host_ports, user, dbname, sslmode,
// sslrootcert and password_env — so ysql_client's effective_tserver_rpc_addrs
// falls through to appending 9100 to each host_ports HOST. Putting both ports on
// one Service is what makes that derivation land somewhere real, and is why this
// is not the operator's headless Service: that one would work too, but then the
// ClusterIP the CR asks for would be created and used by nothing.
// It is a NAME and not a pod list because a full move renames the tserver pods
// and the daemons cannot pick up a new list: the thin client re-resolves its
// configured strings when a connection generation recycles but never re-reads
// config, and the DSN reaches endpoint pods as an env var, which Kubernetes
// never updates in a running container. So the address has to be a name whose
// MEMBERSHIP changes underneath it.
func (d *Deployer) resolveYSQLHost(ctx context.Context) (string, error) {
	name, err := d.tserverServiceName(ctx)
	if err != nil {
		return "", err
	}
	return d.qualify(name), nil
}

// tserverServiceName finds the Service by the labels the chart puts on it.
//
// Not built from a template: see TServerServiceLabels. YBA prefixes a
// Namespaced-scope endpoint with the universe name it generated, which carries
// a suffix nothing here can predict.
func (d *Deployer) tserverServiceName(ctx context.Context) (string, error) {
	res, err := d.kubectlUniverse(ctx, nil, "get", "service", "-l", TServerServiceLabels,
		"-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return "", err
	}
	ns := d.spec.UniverseNamespace()
	names := strings.Fields(res.Stdout)
	switch {
	case res.ExitCode != 0:
		return "", fmt.Errorf("looking for the tserver Service in namespace %s: %s",
			ns, strings.TrimSpace(res.Stderr))
	case len(names) == 0:
		return "", fmt.Errorf("no Service matching %s in namespace %s: the chart did not render the "+
			"%q serviceEndpoint. That namespace is the provider zones' kubeNamespace — if the "+
			"universe went somewhere else, YBA invented a namespace, which it does when no zone "+
			"sets one. Otherwise check that YBA left oldNamingStyle off and that "+
			"spec.enableLoadBalancer came through as true",
			TServerServiceLabels, ns, TServerEndpointName)
	case len(names) > 1:
		// A Namespaced-scope endpoint is meant to be ONE Service shared by
		// every release in the namespace, so more than one means two universes
		// disagree about it and the DSN would be a coin flip.
		return "", fmt.Errorf("%d Services match %s in namespace %s (%s); a Namespaced endpoint "+
			"should be a single shared Service, so two universes are fighting over it",
			len(names), TServerServiceLabels, ns, strings.Join(names, ", "))
	}
	return names[0], nil
}

// qualify renders a Service's in-cluster DNS name.
// The namespace is the UNIVERSE's, not the foundation's: those differ whenever a
// provider zone names its own kubeNamespace, and the DSN has to point where the
// tservers actually are.
func (d *Deployer) qualify(service string) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", service, d.spec.UniverseNamespace())
}

// ensureDatabases creates each requested database.
//
// The amp controller's db.Connect targets yb_storage AT STARTUP, so this must
// finish before the controller rolls out — which is why it is a step of the
// bring-up rather than something the consumer does.
func (d *Deployer) ensureDatabases(ctx context.Context) error {
	if len(d.spec.Databases) == 0 {
		return nil
	}
	service, err := d.tserverServiceName(ctx)
	if err != nil {
		return err
	}
	selector, err := d.tserverSelector(ctx, service)
	if err != nil {
		return err
	}
	pod, err := d.tserverPod(ctx, selector)
	if err != nil {
		return err
	}
	// ysqlsh runs INSIDE the cluster, so it addresses the Service locally
	// whatever scope consumers were told to use: a clusterset name would be a
	// pointless detour through MCS from a pod sitting next to the tservers.
	host := d.qualify(service)
	for _, db := range d.spec.Databases {
		if err := d.createDatabase(ctx, pod, host, db); err != nil {
			return err
		}
	}
	return nil
}

// tserverSelector reads the pod selector off the Service the chart created.
//
// Read rather than guessed: the chart's selector is the authoritative answer to
// "which pods are tservers". Its label key depends on the chart's oldNamingStyle
// value, and in a namespace holding two universes a hand-written selector could
// send the ysqlsh exec to the wrong one.
func (d *Deployer) tserverSelector(ctx context.Context, service string) (map[string]string, error) {
	res, err := d.kubectlUniverse(ctx, nil, "get", "service", service, "-o", "jsonpath={.spec.selector}")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("Service %s not found in namespace %s: %s",
			service, d.spec.UniverseNamespace(), strings.TrimSpace(res.Stderr))
	}
	var selector map[string]string
	if err := json.Unmarshal([]byte(res.Stdout), &selector); err != nil {
		return nil, fmt.Errorf("parse the selector of Service %s: %w", service, err)
	}
	if len(selector) == 0 {
		return nil, fmt.Errorf("Service %s has no selector, so there is no way to tell which pods "+
			"are tservers", service)
	}
	return selector, nil
}

// tserverPod finds a tserver pod to run ysqlsh in.
func (d *Deployer) tserverPod(ctx context.Context, selector map[string]string) (string, error) {
	res, err := d.kubectlUniverse(ctx, nil, "get", "pods", "-l", selectorArg(selector),
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil {
		return "", err
	}
	pod := strings.TrimSpace(res.Stdout)
	if res.ExitCode != 0 || pod == "" {
		return "", fmt.Errorf("no pod matching %q in namespace %s",
			selectorArg(selector), d.spec.UniverseNamespace())
	}
	return pod, nil
}

// selectorArg renders a label set as kubectl's -l argument, sorted so the argv
// is stable and testable.
func selectorArg(selector map[string]string) string {
	keys := make([]string, 0, len(selector))
	for k := range selector {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+selector[k])
	}
	return strings.Join(parts, ",")
}

// createDatabase creates one database, tolerating one that already exists.
//
// Postgres has no CREATE DATABASE IF NOT EXISTS, so the recipe is the one
// amp/deploy/up.sh already uses: probe pg_database, and treat an "already
// exists" error from a racing create as success. A genuine connection failure
// aborts rather than being misread as "absent" and triggering a spurious
// create.
//
// ysqlsh is looked up rather than assumed on PATH: on a YBA-managed universe it
// lives under the tserver's own Postgres install
// (amp/deploy/meko-dev/amp-redeploy.sh does the same).
func (d *Deployer) createDatabase(ctx context.Context, pod, host, db string) error {
	script := fmt.Sprintf(`set -euo pipefail
Y=$(ls /home/yugabyte/tserver/postgres/bin/ysqlsh 2>/dev/null || command -v ysqlsh || echo ysqlsh)
exists=$("$Y" -h %[1]q -p %[2]d -U yugabyte -d yugabyte -tAc "SELECT 1 FROM pg_database WHERE datname='%[3]s'")
if [ "$exists" = "1" ]; then
  echo "exists"
elif out=$("$Y" -h %[1]q -p %[2]d -U yugabyte -d yugabyte -c "CREATE DATABASE %[3]s" 2>&1); then
  echo "created"
elif printf '%%s' "$out" | grep -q "already exists"; then
  echo "exists"
else
  printf 'CREATE DATABASE %[3]s failed: %%s\n' "$out" >&2
  exit 1
fi
`, host, DefaultYSQLPort, db)

	res, err := d.kubectlUniverse(ctx, []byte(script), "exec", "-i", pod, "-c", "yb-tserver",
		"--", "bash", "-s")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("create database %s: %s", db, strings.TrimSpace(res.Stderr+res.Stdout))
	}
	d.log("database %s: %s", db, strings.TrimSpace(res.Stdout))
	return nil
}

// sslMode is the DSN's sslmode, read off the universe body.
//
// There is only one body to read: universeDefaults fills enableClientToNodeEncrypt
// in at PARSE time, so the YBUniverse entry's Spec is the map that was applied.
// It was not always so — the defaults were once applied into a copy at render
// time, and this read the raw one, answering for a universe that was never
// applied. Both directions of that mismatch are a DSN the database
// refuses — `disable` against a TLS universe gives
//
//	FATAL: no pg_hba.conf entry for host "...", user "yugabyte",
//	       database "yb_storage", no encryption (SQLSTATE 28000)
//
// and `require` against a plaintext one fails just as hard. One source for both
// the CR and the DSN is the only way they cannot disagree.
//
// `require`, not `verify-full`: YBA issues its own CA by default and nothing in
// the deploy path distributes it, so demanding verification would swap one
// broken DSN for another. Encryption without authentication of the server is
// what is actually available here.
func (d *Deployer) sslMode() string {
	if tls, _ := d.spec.universe()["enableClientToNodeEncrypt"].(bool); tls {
		return "require"
	}
	return "disable"
}
