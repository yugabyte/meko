package foundation

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"meko/foundation/ybuniverse"
)

// TestExampleSANNamesTheEndpointService: the reported DSN is verify-full
// against the endpoint Service's name, and that name is in none of the SANs the
// chart computes for itself — those are the per-pod names and a wildcard over
// the HEADLESS Service. So the extraSANs override is not decoration, it is the
// difference between consumers connecting and not.
//
// Asserted through the constant rather than the literal because the two have to
// move together: renaming TServerEndpointName without the SAN leaves a spec
// that validates, renders, deploys, and then fails every consumer's hostname
// check forty minutes later.
func TestExampleSANNamesTheEndpointService(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	crSpec := renderedSpec(t, spec, ybuniverse.KindUniverse)

	overrides, _ := crSpec["kubernetesOverrides"].(map[string]any)
	tls, ok := overrides["tls"].(map[string]any)
	if !ok {
		t.Fatalf("kubernetesOverrides has no tls block: %v", overrides)
	}
	sans, _ := tls["extraSANs"].(map[string]any)
	names := asList(sans["dnsNames"])
	if len(names) == 0 {
		t.Fatalf("tls.extraSANs.dnsNames is empty: %v", tls)
	}
	san, _ := names[0].(string)
	for _, want := range []string{
		TServerEndpointName,
		// The chart computes the name, because YBA generates its prefix.
		`include "yugabyte.servicename"`,
		`"scope" "Namespaced"`,
		".Release.Namespace",
	} {
		if !strings.Contains(san, want) {
			t.Errorf("the SAN %q does not carry %q, so it cannot resolve to the Service consumers "+
				"dial", san, want)
		}
	}
	if spec.SSLMode() != "verify-full" {
		t.Errorf("SSLMode = %q: the SAN above only matters because the DSN checks the hostname",
			spec.SSLMode())
	}
}

// TestValidateRequiresTheAddressContract covers the class of spec that would
// build a healthy universe serving no address. None of it is advice: without
// the Service this deployer cannot itself finish — connstr resolves nothing and
// the databases step fails — so a spec that omits it is refused, not warned at.
func TestValidateRequiresTheAddressContract(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{{
		// The shape a spec written before the deployer stopped defaulting has.
		name: "says nothing at all",
		body: "",
		want: "declares no \"yb-tserver-conn\" entry",
	}, {
		name: "no endpoint",
		body: "    enableClientToNodeEncrypt: false\n    enableNodeToNodeEncrypt: false\n",
		want: `declares no "yb-tserver-conn" entry`,
	}, {
		name: "the gate the whole template hangs on",
		body: strings.Replace(contractBody, "enableLoadBalancer: true", "enableLoadBalancer: false", 1),
		want: "enableLoadBalancer is not true",
	}, {
		// One Service per zone release, under a name the labels do not match.
		name: "AZ scope",
		body: strings.Replace(contractBody, "scope: Namespaced", "scope: AZ", 1),
		want: "want Namespaced",
	}, {
		// The chart renders an endpoint only where the labels agree.
		name: "an app the chart has no Service for",
		body: strings.Replace(contractBody, "app: yb-tserver", "app: yb-master", 1),
		want: `want "yb-tserver"`,
	}, {
		// The lane that cannot be addressed separately: it appends 9100 to the
		// YSQL host, so a Service without it points at nothing.
		name: "no RPC port",
		body: strings.Replace(contractBody, "            tcp-rpc-port: \"9100\"\n", "", 1),
		want: "tcp-rpc-port",
	}, {
		name: "the RPC port moved",
		body: strings.Replace(contractBody, `tcp-rpc-port: "9100"`, `tcp-rpc-port: "9200"`, 1),
		want: "tcp-rpc-port",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load([]byte(bareSpec(tc.body)))
			if err == nil {
				t.Fatalf("the spec was accepted; want an error naming %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

// TestEncryptionIsNobodysBusinessButTheOperators: whether a universe is
// encrypted is the operator's call, so a spec that says nothing about either
// flag is accepted — and the deployer answers for the universe that WILL exist,
// not for the fields the file happens to mention.
//
// That second half is why CRDEncryptionDefault is mirrored at all: reading an
// absent flag as false handed a plaintext DSN to an encrypted universe.
func TestEncryptionIsNobodysBusinessButTheOperators(t *testing.T) {
	body := endpointBody // neither flag stated
	spec, err := Load([]byte(bareSpec(body)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Nothing was written into the universe...
	for _, f := range []string{"enableClientToNodeEncrypt", "enableNodeToNodeEncrypt"} {
		if _, set := spec.universe()[f]; set {
			t.Errorf("the deployer wrote universe.%s, which the spec never mentioned", f)
		}
	}
	// ...and the DSN still describes the universe the CRD is about to build.
	if got := spec.TLS(); !got.ClientToNode || !got.NodeToNode || got.SSLMode != "require" {
		t.Errorf("TLS() = %+v, want both flags true and sslmode=require", got)
	}
}

// TestTheExamplesAreValid: both examples have to be ones a reader can copy, and
// Load is now the whole check.
func TestTheExamplesAreValid(t *testing.T) {
	for _, name := range []string{"amp.yaml", "resources.yaml"} {
		t.Run(name, func(t *testing.T) { loadExample(t, name) })
	}
}

// TestAPortWrittenAsANumberIsAccepted: the chart takes either, so a spec that
// leaves the quotes off is not wrong and must not be refused.
func TestAPortWrittenAsANumberIsAccepted(t *testing.T) {
	body := strings.NewReplacer(`"5433"`, "5433", `"9100"`, "9100").Replace(contractBody)
	if _, err := Load([]byte(bareSpec(body))); err != nil {
		t.Errorf("Load: %v", err)
	}
}

// TestEndpointsBesideTheContractAreTheAuthorsBusiness: adding a Service next to
// it — an internal LoadBalancer, a per-AZ endpoint — is exactly what declaring
// the list is for. Only the tserver-conn entry is checked.
func TestEndpointsBesideTheContractAreTheAuthorsBusiness(t *testing.T) {
	body := contractBody + `        - name: yb-master-ui
          type: LoadBalancer
          scope: AZ
          app: yb-master
          ports:
            http-ui: "7000"
`
	spec, err := Load([]byte(bareSpec(body)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	eps := asList(spec.universe()["kubernetesOverrides"].(map[string]any)["serviceEndpoints"])
	if len(eps) != 2 {
		t.Errorf("serviceEndpoints = %v, want both entries applied verbatim", eps)
	}
}

// TestTheContractIsAppliedVerbatim: a LoadBalancer for cross-cluster reach,
// exportService for MCS — applied exactly as written, since nothing is filled
// in and only the fields the address contract needs are checked.
func TestTheContractIsAppliedVerbatim(t *testing.T) {
	body := strings.Replace(contractBody, "          type: ClusterIP\n",
		"          type: LoadBalancer\n          exportService: true\n", 1)
	spec, err := Load([]byte(bareSpec(body)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ep := asList(spec.universe()["kubernetesOverrides"].(map[string]any)["serviceEndpoints"])[0].(map[string]any)
	if ep["type"] != "LoadBalancer" || ep["exportService"] != true {
		t.Errorf("the author's entry was not applied as written: %v", ep)
	}
}

// TestTwoEntriesFightOverOneService: a Namespaced endpoint is one Service
// shared by every release in the namespace, so duplicates make the address a
// coin flip. tserverServiceName errors on this at run time; saying it here is
// forty minutes earlier.
func TestTwoEntriesFightOverOneService(t *testing.T) {
	body := contractBody + `        - name: yb-tserver-conn
          type: LoadBalancer
          scope: Namespaced
          app: yb-tserver
          ports:
            tcp-ysql-port: "5433"
            tcp-rpc-port: "9100"
`
	_, err := Load([]byte(bareSpec(body)))
	if err == nil || !strings.Contains(err.Error(), "2 entries named") {
		t.Errorf("Load = %v, want an error naming the duplicate", err)
	}
}

// TestValidateRootCA: unlike the address contract, these two ARE refusals.
// A spec that names a CA has made a decision about how the universe is signed,
// and both of these mean it cannot come true — YBA discovers that inside a task
// tens of minutes later, with nothing written back to the CR.
func TestValidateRootCA(t *testing.T) {
	const withIssuer = `
    regions:
      - code: r
        zones:
          - code: z
            cloudInfo:
              certManagerIssuerKind: ClusterIssuer
              certManagerIssuerName: amp-ca
`
	for _, tc := range []struct {
		name     string
		provider string
		universe string
		want     string
	}{{
		name:     "a CA nothing uses",
		provider: withIssuer,
		universe: "    rootCA: amp-ca\n    enableClientToNodeEncrypt: false\n" +
			"    enableNodeToNodeEncrypt: false\n",
		want: "both encryption flags are false",
	}, {
		name:     "a CA nothing signs with",
		provider: " {}\n",
		universe: "    rootCA: amp-ca\n    enableClientToNodeEncrypt: true\n" +
			"    enableNodeToNodeEncrypt: false\n",
		want: "certManagerIssuerName",
	}, {
		name:     "coherent",
		provider: withIssuer,
		universe: "    rootCA: amp-ca\n    enableClientToNodeEncrypt: true\n" +
			"    enableNodeToNodeEncrypt: false\n",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			// Load validates, so this is the error an author actually sees.
			_, err := Load([]byte("namespace: amp\nYBProvider:\n  name: p\n  spec:" + tc.provider +
				"YBUniverse:\n  name: foundation\n  spec:\n" + versionLine + tc.universe + endpointBody))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("the spec was accepted; want an error naming %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

// TestCreateDatabaseAuthenticatesWithTheNodeCertificate: with
// `hostssl all yugabyte all cert` in the hba rules, the yugabyte role is a
// certificate login and a bare ysqlsh is refused — so the bring-up would fail
// at its last step, on a universe that is otherwise perfectly healthy.
//
// It does NOT reliably catch a missing SAN, though it once claimed to: where the
// CA is unreadable — which is the case on a YBA-managed pod — it drops to
// `require`, and `require` verifies nothing. The workflow checks the SAN.
func TestCreateDatabaseAuthenticatesWithTheNodeCertificate(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get service -l " + TServerServiceLabels, Result: Result{Stdout: ybaServiceName}},
		{Match: "jsonpath={.spec.selector}", Result: Result{Stdout: tserverSelectorJSON}},
		{Match: "get pods -l", Result: Result{Stdout: "yb-tserver-0"}},
		{Match: "exec", Result: Result{Stdout: "exists\n"}},
	}}
	d, _ := testDeployer(t, rec)
	if err := d.ensureDatabases(context.Background()); err != nil {
		t.Fatalf("ensureDatabases: %v", err)
	}
	script := execStdin(t, rec)
	for _, want := range []string{
		"SSLMODE=verify-full",
		"/root/.yugabytedb/root.crt",
		"/root/.yugabytedb/yugabytedb.key",
		// And a degraded path, valid because no spec here makes yugabyte a
		// certificate login: the CA alone, then plain require.
		"sslmode=require",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the create script is missing %q:\n%s", want, script)
		}
	}
}

// TestCreateDatabaseStaysPlaintextOnAPlaintextUniverse: the same script must
// not offer certificates to a database that will not do TLS at all — that
// fails the connection just as hard in the other direction.
func TestCreateDatabaseStaysPlaintextOnAPlaintextUniverse(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get service -l " + TServerServiceLabels, Result: Result{Stdout: ybaServiceName}},
		{Match: "jsonpath={.spec.selector}", Result: Result{Stdout: tserverSelectorJSON}},
		{Match: "get pods -l", Result: Result{Stdout: "yb-tserver-0"}},
		{Match: "exec", Result: Result{Stdout: "exists\n"}},
	}}
	d, spec := testDeployer(t, rec)
	spec.universe()["enableClientToNodeEncrypt"] = false
	delete(spec.universe(), "rootCA")
	if err := d.ensureDatabases(context.Background()); err != nil {
		t.Fatalf("ensureDatabases: %v", err)
	}
	script := execStdin(t, rec)
	if !strings.Contains(script, "SSLMODE=disable") {
		t.Errorf("the create script does not carry the universe's own sslmode:\n%s", script)
	}
}

// TestDSNCarriesTheConsumerCertificatePaths: the paths are the CONSUMER's, so
// they arrive as connstr flags rather than spec fields — but the DSN they land
// in is still assembled here, because this is the only place that knows its
// shape. Order is fixed: the DSN reaches the amp controller as a Helm value,
// and a re-ordered one would make every converge look like a change.
func TestDSNCarriesTheConsumerCertificatePaths(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get ybuniverse", Result: Result{Stdout: readyStatus()}},
		{Match: "get service -l " + TServerServiceLabels, Result: Result{Stdout: ybaServiceName}},
	}}
	d, _ := testDeployer(t, rec)
	d.WithDSNOptions(DSNOptions{
		SSLRootCert: "/etc/amp/tls/ca/ca.crt",
		SSLCert:     "/etc/amp/tls/client/tls.crt",
		SSLKey:      "/etc/amp/tls/client/tls.key",
	})
	info, err := d.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	want := "?sslmode=verify-full&sslrootcert=%2Fetc%2Famp%2Ftls%2Fca%2Fca.crt" +
		"&sslcert=%2Fetc%2Famp%2Ftls%2Fclient%2Ftls.crt&sslkey=%2Fetc%2Famp%2Ftls%2Fclient%2Ftls.key"
	if !strings.HasSuffix(info.DBURL, want) {
		t.Errorf("DBURL = %q, want it to end %q", info.DBURL, want)
	}
	// And the namespace the pods are in, which a caller that has to exec into a
	// tserver would otherwise re-derive by splitting the host.
	if !strings.Contains(info.ShellExports(), "YB_UNIVERSE_NAMESPACE=") {
		t.Errorf("ShellExports does not carry the universe namespace:\n%s", info.ShellExports())
	}
}

// execStdin is the script the one exec call carried.
func execStdin(t *testing.T, rec *Recorder) string {
	t.Helper()
	for _, c := range rec.Calls {
		if strings.Contains(strings.Join(c.Argv, " "), " exec ") {
			return c.Stdin
		}
	}
	t.Fatalf("no exec call in %v", rec.Argvs())
	return ""
}

// TestUpSaysWhenEncryptionWasInherited: absent is a decision the file does not
// record, and both flags are create-time only — so the one moment it is still
// cheap to change is before anything is applied.
func TestUpSaysWhenEncryptionWasInherited(t *testing.T) {
	spec, err := Load([]byte(bareSpec(endpointBody)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var logged []string
	rec := &Recorder{Responses: []Response{
		{Match: "version --client", Result: Result{}},
		{Match: "get namespace", Result: Result{}},
	}}
	d := New(spec, rec.Runner()).WithLogger(func(f string, a ...any) {
		logged = append(logged, fmt.Sprintf(f, a...))
	})
	// Preflight fails further down without a cluster; the notice comes first,
	// which is the point of where it sits.
	_ = d.Preflight(context.Background())

	got := strings.Join(logged, "\n")
	for _, want := range []string{
		"enableClientToNodeEncrypt is not set",
		"CLIENT connections will be encrypted",
		"sslmode=require",
		"enableNodeToNodeEncrypt is not set",
		"tserver RPC port will be encrypted",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("preflight did not say %q:\n%s", want, got)
		}
	}
}

// TestUpIsQuietWhenTheSpecSaidSo: a spec that stated both flags has nothing to
// be told.
func TestUpIsQuietWhenTheSpecSaidSo(t *testing.T) {
	spec, err := Load([]byte(minimalSpec("")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var logged []string
	rec := &Recorder{Responses: []Response{{Match: "version --client", Result: Result{}}}}
	d := New(spec, rec.Runner()).WithLogger(func(f string, a ...any) {
		logged = append(logged, fmt.Sprintf(f, a...))
	})
	_ = d.Preflight(context.Background())
	if got := strings.Join(logged, "\n"); strings.Contains(got, "is not set") {
		t.Errorf("preflight warned about a spec that stated both flags:\n%s", got)
	}
}

// TestCheckTServerCertSANsReportsBothOutcomes: the SAN is the silent
// 40-minute failure this PR exists to catch, and neither outcome was covered —
// every other Recorder returns empty stdout for `get certificate`, which is the
// early return.
func TestCheckTServerCertSANsReportsBothOutcomes(t *testing.T) {
	const fqdn = "foundation-1954916679-yb-tserver-conn.amp.svc.cluster.local"
	for _, tc := range []struct {
		name     string
		dnsNames string
		want     string
	}{
		{"the SAN landed", `["` + fqdn + `","other"]`, "node certificates name " + fqdn},
		{"the SAN did not", `["yb-tserver-0.yb-tservers.amp.svc.cluster.local"]`,
			"no node certificate names " + fqdn},
		// YBA can sign in-template instead, which renders no Certificate at
		// all: exit 0, empty stdout, and nothing to claim either way.
		{"no cert-manager Certificates", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &Recorder{Responses: []Response{
				{Match: "get certificate", Result: Result{Stdout: tc.dnsNames}},
			}}
			var logged []string
			d, _ := testDeployer(t, rec)
			d.WithLogger(func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) })
			d.checkTServerCertSANs(context.Background(), fqdn)

			got := strings.Join(logged, "\n")
			if tc.want == "" {
				if got != "" {
					t.Errorf("said something about a universe with no Certificates:\n%s", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("logged %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

// TestWaitCertificateHoldsUntilSuccess: YBCertificate reports {success,
// message, resourceUUID} and no state, so waitRegistered's Ready/Error switch
// never fires for it — this is the wait that does.
func TestWaitCertificateHoldsUntilSuccess(t *testing.T) {
	t.Run("registers", func(t *testing.T) {
		rec := &Recorder{Responses: []Response{
			{Match: "get ybcertificate", Result: Result{Stdout: `{"status":{"success":true,"resourceUUID":"c1"}}`}},
		}}
		d, _ := testDeployer(t, rec)
		if err := d.waitCertificate(context.Background(), "amp-ca"); err != nil {
			t.Fatalf("waitCertificate: %v", err)
		}
	})
	t.Run("never succeeds", func(t *testing.T) {
		// No state enum means a rejected certificate and a slow one look the
		// same, so the timeout is the only backstop — and its message has to
		// name what the operator should go and read.
		rec := &Recorder{Responses: []Response{
			{Match: "get ybcertificate", Result: Result{Stdout: `{"status":{"message":"secret not found"}}`}},
		}}
		d, _ := testDeployer(t, rec)
		err := d.waitCertificate(context.Background(), "amp-ca")
		if err == nil {
			t.Fatal("waitCertificate returned nil for a certificate that never registered")
		}
		for _, want := range []string{"did not register", "secret not found", "rootCA"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})
}

// TestPreflightRefusesEncryptionThatDisagreesWithTheUniverse: the upgrade path.
// A deployer that defaulted both flags to false built plaintext universes from
// silent specs and wrote the false into the CR; this one reads an absent flag
// as the CRD's true, so the same spec now means the opposite. Both flags are
// create-time only, so the converge cannot settle it.
func TestPreflightRefusesEncryptionThatDisagreesWithTheUniverse(t *testing.T) {
	// The spec says nothing, so it implies true; the live universe is plaintext.
	spec, err := Load([]byte(bareSpec(endpointBody)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rec := &Recorder{Responses: []Response{
		{Match: "version --client", Result: Result{}},
		{Match: "get ybuniverse", Result: Result{Stdout: "false false"}},
	}}
	d := New(spec, rec.Runner()).WithLogger(func(string, ...any) {})
	err = d.Preflight(context.Background())
	if err == nil {
		t.Fatal("Preflight accepted a spec that implies TLS against a plaintext universe")
	}
	for _, want := range []string{"enableClientToNodeEncrypt", "create-time only", "says nothing"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestPreflightAcceptsEncryptionThatAgrees: the same spec against the universe
// it actually describes.
func TestPreflightAcceptsEncryptionThatAgrees(t *testing.T) {
	spec, err := Load([]byte(minimalSpec(""))) // states both flags false
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rec := &Recorder{Responses: []Response{
		{Match: "version --client", Result: Result{}},
		{Match: "get ybuniverse", Result: Result{Stdout: "false false"}},
		{Match: "get namespace", Result: Result{ExitCode: 1, Stderr: "no cluster"}},
	}}
	d := New(spec, rec.Runner()).WithLogger(func(string, ...any) {})
	// It fails later, on the cluster it cannot reach — the point is that it got
	// past the encryption check.
	if err := d.Preflight(context.Background()); err != nil &&
		strings.Contains(err.Error(), "create-time only") {
		t.Errorf("Preflight refused a spec that matches the universe: %v", err)
	}
}

// TestWaitCertificateFailsFastOnARejection: a real run spent 45 minutes
// re-reading "Failed to process certificate: Certificate must end with a
// newline" before reporting it. YBA does say why it refused, so say it back at
// once — and quote it, since the heuristic is on prose.
func TestWaitCertificateFailsFastOnARejection(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "get ybcertificate", Result: Result{Stdout: `{"status":{"success":false,` +
			`"message":"Failed to process certificate: Certificate must end with a newline"}}`}},
	}}
	d, _ := testDeployer(t, rec)
	start := time.Now()
	err := d.waitCertificate(context.Background(), "amp-ca")
	if err == nil {
		t.Fatal("waitCertificate waited out a rejection it could have reported")
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("took %s to report a rejection that was already in the status", took)
	}
	for _, want := range []string{"was rejected", "must end with a newline", "certificateSecretRef"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q", err, want)
		}
	}
}
