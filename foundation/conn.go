package foundation

import (
	"context"
	"fmt"
	"net/url"
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
	// DBURL is a DSN for ReportedDatabase, which is what the amp controller
	// takes as DB_URL. Empty when it could not be completed — see dsn.
	DBURL string `json:"db_url,omitempty"`
	// SSLMode is what the universe implies, from its encryption flags and
	// whether a rootCA names a CA a consumer can verify against. Reported
	// alongside DBURL and not only inside it, because DBURL is absent whenever
	// it could not be completed — and this is the field a consumer needs to
	// build its own.
	SSLMode string `json:"ssl_mode"`
	// SQLEndpoints is what the CR reports in status.sqlEndpoints, carried for
	// diagnostics only. It is deliberately NOT used as YSQLHostPorts: it may
	// enumerate per-pod endpoints, and a per-pod list is exactly what a full
	// move invalidates.
	SQLEndpoints []string `json:"sql_endpoints,omitempty"`
	// UniverseNamespace is where the DB pods actually are — the provider zones'
	// kubeNamespace, which need not be the foundation's. Exported because a
	// caller that has to exec into a tserver (deploy/tls/yb-storage-role.sh)
	// would otherwise have to re-derive it by splitting the host.
	UniverseNamespace string `json:"universe_namespace"`
}

// ReportedDatabase is the database the reported DSN names.
//
// A constant, not a spec field. It was one, and the field earned nothing: the
// name travelled into the spec and straight back out, since a consumer always
// knows which database it wants. Hardcoding it costs a consumer that wants
// another name nothing either — the DSN is a convenience, and the parts beside
// it (YB_HOST_PORTS, and YB_SSLMODE in the ConfigMap) are what a consumer
// assembles its own from.
//
// It is amp's database. meko uses meko_system and a per-datapack name, so when
// meko moves onto a foundation it will build its own DSN from the parts rather
// than this, and nothing here has to change for it.
const ReportedDatabase = "yb_storage"

// DSNOptions are libpq parameters the CONSUMER supplies. They are paths inside
// the consumer's own container, so the spec cannot know them and deliberately
// does not carry them: which address and which files a consumer uses are
// properties of its topology, not of the universe. They arrive as connstr
// flags and land here, because Info.DBURL is the one place that knows what a
// DSN for this foundation looks like — assembling it a second time in bash is
// how the two drift.
type DSNOptions struct {
	SSLMode     string
	SSLRootCert string
	SSLCert     string
	SSLKey      string
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
	if i.UniverseNamespace != "" {
		fmt.Fprintf(&b, "export YB_UNIVERSE_NAMESPACE=%q\n", i.UniverseNamespace)
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

		UniverseNamespace: d.spec.UniverseNamespace(),
		SSLMode:           d.SSLMode(),
	}
	info.DBURL = d.dsn(info.YSQLHostPorts, ReportedDatabase)
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

// sslMode is the DSN's sslmode, read off the universe body.
//
// There is only one body to read: the YBUniverse entry's Spec is applied
// verbatim, so what this reads is what the universe got. It was not always so —
// defaults were once written into a copy at render time, and this read the raw
// one, answering for a universe that was never applied. Both directions of that
// mismatch are a DSN the database refuses — `disable` against a TLS universe
// gives
//
//	FATAL: no pg_hba.conf entry for host "...", user "yugabyte",
//	       database "yb_storage", no encryption (SQLSTATE 28000)
//
// and `require` against a plaintext one fails just as hard. One source for both
// the CR and the DSN is the only way they cannot disagree.
//
// Which TLS mode depends on whose CA signed the node certificates, because
// that is what decides whether anything can be verified:
//
//   - no rootCA — YBA invents its own CA and nothing in the deploy path
//     distributes it, so `verify-full` would swap one broken DSN for another.
//     `require` is encryption without authentication of the server, which is
//     what is actually available.
//
//   - rootCA set — the universe is signed by a CA the spec named rather than
//     one YBA invented, so the server can be authenticated and `verify-full` is
//     the honest answer. It also checks the HOSTNAME, which is why the spec's
//     tls.extraSANs has to name the endpoint Service: that name is the only
//     address consumers get, and it is not one the chart puts in the
//     certificate on its own.
//
//     Naming a CA is taken as the intent that consumers have it — the usual
//     case being deploy/tls/trust-root.sh, which puts it in every namespace it
//     labels. NOTHING CHECKS THAT, and nothing here can: whether a consumer
//     holds the CA is a fact about the consumer. A rootCA pointing at a
//     SELF_SIGNED YBCertificate reports verify-full against a CA nobody has,
//     and the failure is a handshake error at first connect. Checking it would
//     mean Validate reading the named YBCertificate's certType.
//
// A caller may override it — `connstr --sslmode` — for a consumer that has the
// CA somewhere this deployer cannot see.
func (d *Deployer) SSLMode() string {
	if d.dsnOpts.SSLMode != "" {
		return d.dsnOpts.SSLMode
	}
	return d.spec.SSLMode()
}

func (d *Deployer) sslMode() string { return d.SSLMode() }

// dsn renders the DSN for one database: the universe's own sslmode, plus
// whatever certificate paths the consumer supplied.
//
// It returns EMPTY when the sslmode requires a CA and the caller named none,
// because such a DSN is worse than no DSN: libpq falls back to
// ~/.postgresql/root.crt, which no consumer of this has, so the string parses
// and then fails at the first connection.
//
// `connstr` refuses that case outright, with a message naming the fix, so a
// human asking for a DSN is told why. The reconcile loop is the caller that
// reaches here — it runs in a pod with no flags for anyone to pass — and it
// reports no DSN at all rather than one that cannot work. A consumer in the
// cluster reads the parts from the foundation-conn ConfigMap instead.
//
// The parameter order is fixed rather than map-ranged, because the DSN reaches
// the amp controller as a Helm value and a re-ordered one would make every
// converge look like a change.
func (d *Deployer) dsn(hostPorts, db string) string {
	if strings.HasPrefix(d.sslMode(), "verify") && d.dsnOpts.SSLRootCert == "" {
		return ""
	}
	params := []string{"sslmode=" + url.QueryEscape(d.sslMode())}
	for _, p := range []struct{ key, value string }{
		{"sslrootcert", d.dsnOpts.SSLRootCert},
		{"sslcert", d.dsnOpts.SSLCert},
		{"sslkey", d.dsnOpts.SSLKey},
	} {
		if p.value != "" {
			params = append(params, p.key+"="+url.QueryEscape(p.value))
		}
	}
	return fmt.Sprintf("postgres://yugabyte@%s/%s?%s", hostPorts, db, strings.Join(params, "&"))
}
