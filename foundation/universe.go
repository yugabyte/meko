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
		if m.Kind == ybuniverse.KindProvider {
			if err := d.waitProvider(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

// waitProvider blocks until the provider has registered with YBA.
//
// It is a separate wait rather than folded into the universe's, because YBA
// rejects a universe whose providerName is not a registered provider — and that
// rejection arrives asynchronously as a failed action on the universe, which is
// a much worse place to read "the provider was not ready yet".
func (d *Deployer) waitProvider(ctx context.Context) error {
	name := d.spec.ProviderName()
	deadline := time.Now().Add(d.universeTimeout)
	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()

	var last string
	for {
		res, err := d.kubectl(ctx, nil, "get", "ybprovider", name, "-o", "json")
		if err != nil {
			return err
		}
		if res.ExitCode == 0 {
			var doc struct {
				Status ybuniverse.ProviderStatus `json:"status"`
			}
			if err := json.Unmarshal([]byte(res.Stdout), &doc); err != nil {
				return fmt.Errorf("parse ybprovider %s: %w", name, err)
			}
			st := doc.Status
			switch {
			case strings.EqualFold(st.State, "Ready"):
				d.log("provider %s is registered (%s)", name, st.ResourceUUID)
				return nil
			case strings.EqualFold(st.State, "Error"), strings.EqualFold(st.State, "Failed"):
				msg := st.Message
				if msg == "" {
					msg = "no message"
				}
				return fmt.Errorf("provider %s failed to register: %s", name, msg)
			}
			if reason := st.State + " " + st.Message; reason != last {
				d.log("waiting for provider %s: %s", name, strings.TrimSpace(reason))
				last = reason
			}
		} else if last != "absent" {
			d.log("waiting for provider %s to appear", name)
			last = "absent"
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("provider %s did not register within %s (last: %s)",
				name, d.universeTimeout, strings.TrimSpace(last))
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
// Nothing here creates it: it is declared as a serviceEndpoints entry in the CR
// (see Spec.universeDefaults), and the chart renders it with its own selector. This
// only verifies the result, because two chart values have to line up for that
// endpoint to render at all and neither is ours to set:
//
//   - enableLoadBalancer must be on. The CR sets it, so this should hold.
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
	return nil
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

	var lastReason string
	for {
		status, present, err := d.getUniverseStatus(ctx)
		if err != nil {
			return err
		}
		if present {
			// Check failure before settled: a failed action is terminal, and
			// reporting it now beats waiting out the timeout to say nothing.
			if failed, why := ybuniverse.Failed(status); failed {
				return fmt.Errorf("universe %s: %s", d.spec.UniverseName(), why)
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
			return fmt.Errorf("universe %s did not settle within %s (last: %s)\n%s",
				d.spec.UniverseName(), d.universeTimeout, lastReason, d.diagnose(ctx))
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
