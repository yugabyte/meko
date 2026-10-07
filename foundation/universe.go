package foundation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"meko/foundation/ybuniverse"
)

// applyCRs server-side applies every CR the spec declares, in the order the
// spec declares them.
//
// Two kinds get something extra, and only because the deployer has to do
// something about them rather than because they are privileged: it waits for
// a YBProvider to register, and runs the release check before a YBUniverse.
//
// force=true takes ownership of fields another manager set. Without it, a CR
// somebody once edited with `kubectl edit` comes back as a field-manager
// conflict on every converge, forever — the same reason ycp/cmd/agent/k8s.go
// forces its applies. The consequence is deliberate and worth knowing: the spec
// file wins over a hand edit, so the file is the only place to change the
// foundation.
func (d *Deployer) applyCRs(ctx context.Context) error {
	manifests, err := d.spec.Render()
	if err != nil {
		return err
	}
	// Document order, which Validate has already checked puts any YBProvider
	// above the YBUniverse — YBA cannot place a universe against a provider
	// that does not exist, and it rejects one whose provider has not finished
	// registering, hence the wait rather than just the ordering.
	for _, m := range manifests {
		if m.Kind == ybuniverse.KindUniverse {
			// Last thing before the universe asks for the release, so a
			// Release entry above has already had its chance to import it.
			if err := d.checkDBRelease(ctx); err != nil {
				return err
			}
		}
		d.log("applying %s from %s", m, m.Name)
		res, err := d.kubectl(ctx, m.JSON, "apply", "--server-side", "--force-conflicts", "-f", "-")
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("apply %s: %s", m.Kind, strings.TrimSpace(res.Stderr))
		}
		// Both of these are referenced BY NAME from the universe below, and YBA
		// resolves the reference when it reconciles — asynchronously, with the
		// rejection arriving as a failed action tens of minutes later, or for
		// telemetry as nothing at all. Ordering alone is not enough; the wait is.
		if d.noWaits {
			continue
		}
		switch m.Kind {
		case ybuniverse.KindProvider, ybuniverse.KindTelemetryProvider:
			if err := d.waitRegistered(ctx, m.Kind, m.ObjectName); err != nil {
				return err
			}
		case ybuniverse.KindCertificate:
			if err := d.waitCertificate(ctx, m.ObjectName); err != nil {
				return err
			}
		}
	}
	return nil
}

// waitCertificate blocks until YBA has taken the CA.
//
// Its own wait rather than waitRegistered's, because the status shape differs:
// YBCertificate reports {success, message, resourceUUID} with no `state`, so the
// Ready/Error switch over there never fires and the poll would run out the
// universe timeout on a certificate that registered in seconds.
//
// Two things this learned from a real run, both the hard way.
//
// YBA DOES say why it refused: `Failed to process certificate: Certificate must
// end with a newline` — a PEM through a shell command substitution, which strips
// the trailing newline. So a message beginning "Failed" is treated as final
// rather than progress. It is a heuristic on prose, which is why the deadline
// below is still the backstop and the message is quoted verbatim either way.
//
// And it does not get the universe's timeout. Registering a certificate is
// metadata, not a bring-up: it takes seconds or it is not going to happen, and
// the run that found the newline bug spent 45 minutes re-reading the same
// sentence before saying it.
func (d *Deployer) waitCertificate(ctx context.Context, name string) error {
	timeout := certificateTimeout
	if d.universeTimeout < timeout {
		timeout = d.universeTimeout
	}
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()

	var last string
	for {
		res, err := d.kubectl(ctx, nil, "get", "ybcertificate", name, "-o", "json")
		if err != nil {
			return err
		}
		if res.ExitCode == 0 {
			var doc struct {
				Status struct {
					Success      bool   `json:"success"`
					Message      string `json:"message"`
					ResourceUUID string `json:"resourceUUID"`
				} `json:"status"`
			}
			if err := json.Unmarshal([]byte(res.Stdout), &doc); err != nil {
				return fmt.Errorf("parse ybcertificate %s: %w", name, err)
			}
			if doc.Status.Success {
				d.log("YBCertificate %s is registered (%s)", name, doc.Status.ResourceUUID)
				return nil
			}
			if strings.HasPrefix(strings.TrimSpace(doc.Status.Message), "Failed") {
				return fmt.Errorf("YBCertificate %s was rejected: %s. The universe's rootCA names "+
					"it, so nothing can sign node certificates until it is accepted — the content "+
					"comes from the Secret its certificateSecretRef names",
					name, strings.TrimSpace(doc.Status.Message))
			}
			if doc.Status.Message != last {
				d.log("waiting for YBCertificate %s: %s", name, strings.TrimSpace(doc.Status.Message))
				last = doc.Status.Message
			}
		} else if last != "absent" {
			d.log("waiting for YBCertificate %s to appear", name)
			last = "absent"
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("YBCertificate %s did not register within %s (last: %s). The universe's "+
				"rootCA names it, so YBA cannot sign node certificates until it does; check its "+
				"status.message and that the Secret it references exists",
				name, timeout, strings.TrimSpace(last))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// waitProvider blocks until the provider has registered with YBA.
//
// It is a separate wait rather than folded into the universe's, because YBA
// rejects a universe whose providerName is not a registered provider — and that
// rejection arrives asynchronously as a failed action on the universe, which is
// a much worse place to read "the provider was not ready yet".
func (d *Deployer) waitProvider(ctx context.Context) error {
	return d.waitRegistered(ctx, ybuniverse.KindProvider, d.spec.ProviderName())
}

// waitRegistered blocks until YBA has created the object behind a CR and given
// it a UUID.
//
// One function for YBProvider and TelemetryProvider because their statuses are
// the same three fields and their failure is the same shape: the universe names
// them, YBA resolves that name when it reconciles, and a name that is not ready
// yet is rejected asynchronously. For a provider that surfaces as a failed
// action on the universe tens of minutes in; for telemetry it surfaces as
// nothing whatsoever — the universe is healthy and simply never exports. Which
// is why this waits rather than trusting document order.
func (d *Deployer) waitRegistered(ctx context.Context, kind, name string) error {
	// kubectl takes the lowercased kind; both are singular resources.
	resource := strings.ToLower(kind)
	deadline := time.Now().Add(d.universeTimeout)
	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()

	var last string
	for {
		res, err := d.kubectl(ctx, nil, "get", resource, name, "-o", "json")
		if err != nil {
			return err
		}
		if res.ExitCode == 0 {
			var doc struct {
				Status ybuniverse.ProviderStatus `json:"status"`
			}
			if err := json.Unmarshal([]byte(res.Stdout), &doc); err != nil {
				return fmt.Errorf("parse %s %s: %w", resource, name, err)
			}
			st := doc.Status
			switch {
			case strings.EqualFold(st.State, "Ready"):
				d.log("%s %s is registered (%s)", kind, name, st.ResourceUUID)
				return nil
			case strings.EqualFold(st.State, "Error"), strings.EqualFold(st.State, "Failed"):
				msg := st.Message
				if msg == "" {
					msg = "no message"
				}
				return fmt.Errorf("%s %s failed to register: %s", kind, name, msg)
			}
			if reason := st.State + " " + st.Message; reason != last {
				d.log("waiting for %s %s: %s", kind, name, strings.TrimSpace(reason))
				last = reason
			}
		} else if last != "absent" {
			d.log("waiting for %s %s to appear", kind, name)
			last = "absent"
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s %s did not register within %s (last: %s)",
				kind, name, d.universeTimeout, strings.TrimSpace(last))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// checkTServerService confirms the chart created the Service both compute lanes
// go through.
//
// Nothing here creates it: it is declared as a serviceEndpoints entry in the
// spec, and the chart renders it with its own selector. This only verifies the
// result, because two chart values have to line up for that endpoint to render
// at all and only one of them is in the spec:
//
//   - enableLoadBalancer must be on. The spec sets it and Validate refuses one
//     that does not, so this should hold.
//   - oldNamingStyle must be off, or a Namespaced-scope endpoint is skipped.
//     That is YBA's choice and is NOT in the CRD's kubernetesOverrides schema,
//     so it cannot be forced. Real YBA universes have hash-prefixed Service
//     names, which is the new style, so it is expected to be off.
//
// A miss here means the direct-RPC address resolves to nothing, and the symptom
// downstream is compute daemons failing to open their table handles at startup
// with nothing pointing back at this. So it is worth a loud, specific warning
// rather than silence.
func (d *Deployer) checkTServerService(ctx context.Context) error {
	name, err := d.tserverServiceName(ctx)
	if err != nil {
		// Still a warning, not a failure: the Connect that follows fails on the
		// same lookup with the same explanation, and a universe that is
		// otherwise Ready should not be reported as a broken install by the
		// step whose whole job is to describe it.
		d.log("warning: %v", err)
		return nil
	}
	res, err := d.kubectlUniverse(ctx, nil, "get", "service", name, "-o", "json")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		d.log("warning: Service %s vanished between the lookup and the read", name)
		return nil
	}
	var svc struct {
		Spec struct {
			Type     string            `json:"type"`
			Selector map[string]string `json:"selector"`
			Ports    []struct {
				Name string `json:"name"`
				Port int    `json:"port"`
			} `json:"ports"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &svc); err != nil {
		return fmt.Errorf("parse Service %s: %w", name, err)
	}
	// Both ports matter and for different reasons: YSQL uses the host as given,
	// and the direct-RPC lane derives the same host on 9100. A Service missing
	// either leaves one lane pointing at a port that is not there.
	ports := map[int]bool{}
	for _, p := range svc.Spec.Ports {
		ports[p.Port] = true
	}
	for _, want := range []int{DefaultYSQLPort, TServerRPCPort} {
		if !ports[want] {
			d.log("warning: Service %s does not expose port %d (has %v); one compute lane will have "+
				"nowhere to connect", name, want, svc.Spec.Ports)
		}
	}
	if svc.Spec.Type != "ClusterIP" {
		d.log("warning: Service %s is type %s, not ClusterIP; the direct-RPC lane's load spreading "+
			"assumes a VIP", name, svc.Spec.Type)
	}
	res, err = d.kubectlUniverse(ctx, nil, "get", "endpoints", name,
		"-o", "jsonpath={.subsets[*].addresses[*].ip}")
	if err != nil {
		return err
	}
	if n := len(strings.Fields(res.Stdout)); n == 0 {
		d.log("warning: Service %s has no endpoints yet (selector %v) — expected while the tserver "+
			"pods are still starting, but if it stays empty the compute hot path will fail at startup",
			name, svc.Spec.Selector)
	} else {
		d.log("Service %s: %d endpoint(s) on port %d, selector %v",
			name, n, TServerRPCPort, svc.Spec.Selector)
	}
	d.checkTServerCertSANs(ctx, d.qualify(name))
	return nil
}

// checkTServerCertSANs confirms the node certificates name the address
// consumers are given. It is what notices a missing SAN here rather than
// leaving it to the first consumer that opens a verify-full connection.
//
// It is the one thing about a cert-manager universe that cannot be checked
// offline. The spec asks for the Service FQDN through
// kubernetesOverrides.tls.extraSANs, but YBA writes the universe chart's whole
// tls block itself, and whether our override survives that is YBA's business.
// If it does not, everything looks healthy: the Service exists, the endpoints
// are there, the universe is Ready — and every verify-full consumer fails the
// hostname check against a name that is in no certificate.
//
// The default SAN set is why the override is needed at all: the chart puts the
// per-pod names and a wildcard over the HEADLESS Service in the certificate,
// and this endpoint Service is neither.
//
// A warning, like everything else here, and only for a universe that names a
// rootCA — with YBA's own CA the DSN is `require`, which never checks a name.
func (d *Deployer) checkTServerCertSANs(ctx context.Context, fqdn string) {
	if d.spec.SSLMode() != "verify-full" {
		return
	}
	res, err := d.kubectlUniverse(ctx, nil, "get", "certificate",
		"-o", "jsonpath={.items[*].spec.dnsNames}")
	// No cert-manager Certificates is not itself wrong: YBA can hand the chart
	// a CA and let it sign the node certificates in-template, which takes the
	// same extraSANs. That case exits 0 with EMPTY stdout, so it has to be
	// caught here rather than falling through to the warning below.
	if err != nil || res.ExitCode != 0 || strings.TrimSpace(res.Stdout) == "" {
		return
	}
	if strings.Contains(res.Stdout, fqdn) {
		d.log("node certificates name %s", fqdn)
		return
	}
	d.log("warning: no node certificate names %s, but the DSN this foundation reports is "+
		"sslmode=verify-full against exactly that name, so every consumer will fail the hostname "+
		"check. The spec asks for it through kubernetesOverrides.tls.extraSANs; YBA may not have "+
		"passed it through. Certificate dnsNames: %s", fqdn, strings.TrimSpace(res.Stdout))
}

// getUniverseStatus reads the CR's status. A missing CR reports absent rather
// than failing: during a create the resource may not be visible yet.
func (d *Deployer) getUniverseStatus(ctx context.Context) (ybuniverse.Status, bool, error) {
	res, err := d.kubectl(ctx, nil, "get", "ybuniverse", d.spec.UniverseName(), "-o", "json")
	if err != nil {
		return ybuniverse.Status{}, false, err
	}
	if res.ExitCode != 0 {
		if strings.Contains(res.Stderr, "NotFound") || strings.Contains(res.Stderr, "not found") {
			return ybuniverse.Status{}, false, nil
		}
		return ybuniverse.Status{}, false, fmt.Errorf("get ybuniverse %s: %s",
			d.spec.UniverseName(), strings.TrimSpace(res.Stderr))
	}
	var doc struct {
		Status ybuniverse.Status `json:"status"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &doc); err != nil {
		return ybuniverse.Status{}, false, fmt.Errorf("parse ybuniverse %s: %w", d.spec.UniverseName(), err)
	}
	return doc.Status, true, nil
}

// waitUniverse blocks until the universe has converged on the spec it was just
// given, or fails with the operator's own reason.
//
// The predicate is ybuniverse.Settled, and the reason it is not simply
// "universeState == Ready" is the whole difficulty of this wait: the CRD has no
// observedGeneration, so immediately after an apply the operator has not looked
// at the new spec and the state still reads Ready from the PREVIOUS generation.
// Waiting on state alone returns instantly and reports an edit as applied that
// has not started. status.actions closes that hole because the operator
// enqueues an action before it begins work.
func (d *Deployer) waitUniverse(ctx context.Context) error {
	deadline := time.Now().Add(d.universeTimeout)
	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()

	// Stateful, and one per wait: it reports a failure the first time it sees
	// one and whenever it changes. It ends nothing — YBA retries every failure
	// state it can report, so the timeout below is the only bound (see
	// ybuniverse.FailureWatch).
	var watch ybuniverse.FailureWatch
	var lastReason string
	var hinted bool
	for {
		status, present, err := d.getUniverseStatus(ctx)
		if err != nil {
			return err
		}
		if present {
			if report := watch.Observe(status); report != "" {
				d.log("universe %s: %s", d.spec.UniverseName(), report)
				if !hinted {
					// Once per wait: the reason names the yugaware log, this
					// says how to read it on THIS install.
					d.log("%s", d.yugawareLogHint(ctx))
					hinted = true
				}
			}
			if settled, why := ybuniverse.Settled(status); settled {
				d.log("universe %s is Ready", d.spec.UniverseName())
				return nil
			} else if why != lastReason {
				d.log("waiting for universe %s: %s", d.spec.UniverseName(), why)
				lastReason = why
			}
		} else if lastReason != "absent" {
			d.log("waiting for universe %s to appear", d.spec.UniverseName())
			lastReason = "absent"
		}

		if time.Now().After(deadline) {
			// The failure goes in the message: a timeout that happened while
			// the CR was reporting one is a different thing from a universe
			// that was merely slow, and the reason is what someone reading a
			// CI log has.
			failure := ""
			if r := watch.Reason(); r != "" {
				// The reason says the explanation is in the yugaware log; this
				// says how to read it on THIS install. It rides the error
				// rather than only the log because a CI run keeps the error.
				failure = "\nthe CR was reporting: " + r + "\n" + d.yugawareLogHint(ctx)
			}
			return fmt.Errorf("universe %s did not settle within %s (last: %s)%s\n%s",
				d.spec.UniverseName(), d.universeTimeout, lastReason, failure, d.diagnose(ctx))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// diagnose gathers what someone reading a timeout actually needs. Without it a
// timeout says only that time passed, and the reason — an unschedulable pod, a
// PVC with no volume, a rejected spec — stays in the cluster.
func (d *Deployer) diagnose(ctx context.Context) string {
	var b strings.Builder
	if res, err := d.kubectl(ctx, nil, "get", "ybuniverse", d.spec.UniverseName(), "-o", "yaml"); err == nil {
		b.WriteString("--- ybuniverse " + d.spec.UniverseName() + " ---\n" + res.Stdout + "\n")
	}
	if res, err := d.kubectlUniverse(ctx, nil, "get", "pods", "-o", "wide"); err == nil {
		b.WriteString("--- pods ---\n" + res.Stdout + "\n")
	}
	if res, err := d.kubectlUniverse(ctx, nil, "get", "events", "--sort-by=.lastTimestamp"); err == nil {
		b.WriteString("--- recent events ---\n" + tail(res.Stdout, 40) + "\n")
	}
	return b.String()
}

// tail returns the last n lines, so a diagnostic dump does not bury the recent
// events that matter under an hour of older ones.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
