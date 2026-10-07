package foundation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// The Foundation CRD the reconcile loop reads. The schema is the CRD in
// deploy/helm/meko-foundation-crd, which is the only thing that installs it.
const (
	FoundationGroup = "meko.yugabyte.com"
	// FoundationKind is spelled with its group on every kubectl call, so a
	// cluster that has some other `foundations` resource cannot be what gets
	// read instead.
	FoundationKind = "foundations." + FoundationGroup
	// FoundationFinalizer holds the object open while the universe is torn
	// down. It is added ONLY when deletionPolicy is Delete — with the default
	// policy there is nothing to do on deletion, and a finalizer that can only
	// be cleared by a pod in the namespace being deleted is how a namespace
	// gets stuck in Terminating for good.
	FoundationFinalizer = FoundationGroup + "/teardown"
)

// FoundationCR is the part of the object the loop reads.
//
// The spec stays RAW. Spec's own loader is the only thing that parses it, both
// because the resource list is order-sensitive and because that is what keeps
// one set of rules for a spec whichever way it arrived.
type FoundationCR struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		// UID distinguishes one object from its replacement. A delete and a
		// re-apply under the default Retain policy leave the same name at
		// generation 1, so nothing else does.
		UID               string   `json:"uid"`
		Generation        int64    `json:"generation"`
		Finalizers        []string `json:"finalizers"`
		DeletionTimestamp string   `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec   json.RawMessage `json:"spec"`
	Status struct {
		Conditions []Condition `json:"conditions"`
	} `json:"status"`
}

// Condition is the standard Kubernetes condition shape. LastTransitionTime
// moves only when Status changes, which is what makes "how long has it been
// like this" answerable.
type Condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
}

// Condition types and the Status values they take.
const (
	ConditionReady     = "Ready"
	ConditionConverged = "Converged"

	CondTrue    = "True"
	CondFalse   = "False"
	CondUnknown = "Unknown"
)

func (c *FoundationCR) hasFinalizer() bool {
	for _, f := range c.Metadata.Finalizers {
		if f == FoundationFinalizer {
			return true
		}
	}
	return false
}

// crClient reads and writes one Foundation object.
//
// Separate from Deployer because it exists BEFORE a spec does: this object is
// where the spec comes from, so it cannot be reached through something built
// out of one.
type crClient struct {
	run         Runner
	namespace   string
	name        string
	kubeContext string
}

func (c *crClient) kubectl(ctx context.Context, args ...string) (Result, error) {
	argv := []string{"kubectl"}
	if c.kubeContext != "" {
		argv = append(argv, "--context", c.kubeContext)
	}
	argv = append(argv, "-n", c.namespace)
	return c.run(ctx, nil, append(argv, args...))
}

// get reads the object. A missing one is (nil, nil) rather than an error: the
// chart installs this pod and the Foundation can be written afterwards, so
// "not there yet" is a state to sit in.
func (c *crClient) get(ctx context.Context) (*FoundationCR, error) {
	res, err := c.kubectl(ctx, "get", FoundationKind, c.name, "-o", "json")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		if strings.Contains(res.Stderr, "NotFound") || strings.Contains(res.Stderr, "not found") {
			return nil, nil
		}
		return nil, fmt.Errorf("get %s %s: %s", FoundationKind, c.name, strings.TrimSpace(res.Stderr))
	}
	var cr FoundationCR
	if err := json.Unmarshal([]byte(res.Stdout), &cr); err != nil {
		return nil, fmt.Errorf("decode %s %s: %w", FoundationKind, c.name, err)
	}
	return &cr, nil
}

// writeStatus patches the status SUBRESOURCE, which is what stops a status
// write from ever touching the spec — the object is the user's to edit and the
// loop's only to report on.
func (c *crClient) writeStatus(ctx context.Context, status any) error {
	body, err := json.Marshal(map[string]any{"status": status})
	if err != nil {
		return err
	}
	res, err := c.kubectl(ctx, "patch", FoundationKind, c.name,
		"--subresource=status", "--type=merge", "-p", string(body))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("patch status: %s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

// setFinalizers replaces the list. The whole list is sent because a merge patch
// replaces an array wholesale, so the caller filters rather than removing one
// entry — which also preserves a finalizer somebody else put there.
func (c *crClient) setFinalizers(ctx context.Context, list []string) error {
	// json.Marshal renders a nil slice as null, which a merge patch reads as
	// "delete this key" rather than "empty it". For finalizers those happen to
	// mean the same thing, but only by luck, so send [] and not null.
	if list == nil {
		list = []string{}
	}
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"finalizers": list}})
	if err != nil {
		return err
	}
	res, err := c.kubectl(ctx, "patch", FoundationKind, c.name, "--type=merge", "-p", string(body))
	if err != nil {
		return err
	}
	// The object disappears the moment the last finalizer goes, so kubectl
	// reporting it gone is this call having worked, not having failed.
	if res.ExitCode != 0 && !strings.Contains(res.Stderr, "not found") {
		return fmt.Errorf("patch finalizers: %s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

// loadCRSpec parses the object's spec with the loader the CLI uses on a file.
// The spec arrives as JSON, which is YAML, so there is one parser and one set
// of rules — the strict unknown-field check included — however the spec came in.
func loadCRSpec(cr *FoundationCR) (*Spec, error) {
	if len(cr.Spec) == 0 {
		return nil, fmt.Errorf("the Foundation has no spec")
	}
	// A Foundation knows its own namespace, so the field is optional here in a
	// way it cannot be for a file. Naming a different one still works and is
	// how the universe is put somewhere else.
	spec, err := LoadInNamespace(cr.Spec, cr.Metadata.Namespace)
	if err != nil {
		return nil, err
	}
	if spec.KubeContext != "" {
		// Refused rather than ignored. The loop picked its cluster when it read
		// this object, so the field cannot do what it says — and silently
		// ignoring it would leave somebody believing they had pointed a
		// foundation at another cluster.
		return nil, fmt.Errorf("kubeContext is set to %q, which a Foundation cannot honour: the "+
			"cluster is the one this loop is already running against, chosen at startup",
			spec.KubeContext)
	}
	return spec, nil
}
