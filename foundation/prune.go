package foundation

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// operatorAPIGroup is the group every kind in the bundle belongs to, and the
// only group pruning will ever look at.
const operatorAPIGroup = "operator.yugabyte.io"

// prune deletes every operator CR this deployer created that the spec no
// longer declares.
//
// Without it the spec is not the source of truth: removing a key would stop
// sending an object without removing it, and the cluster would keep a
// KMSConfig or a StorageConfig nothing in the file mentions.
//
// It owns no kinds. The set of kinds comes from the CLUSTER — `kubectl
// api-resources` for the operator's group — rather than from a list here, so a
// kind shipped in a future chart is pruned without a code change, and there is
// nothing to keep in step with the bundle.
//
// Three things keep it from deleting more than it should. The managed-by label
// is the ownership boundary, so a CR somebody else put in the namespace is
// never a candidate. The query is namespace-scoped, so one foundation cannot
// reach another's objects. And a discovery failure is an error rather than an
// empty result, because "I could not list anything" and "there is nothing to
// delete" must not look the same to a delete loop.
func (d *Deployer) prune(ctx context.Context, want []Manifest) error {
	kinds, err := d.operatorKinds(ctx)
	if err != nil {
		return err
	}
	have, err := d.ownedObjects(ctx, kinds)
	if err != nil {
		return err
	}

	keep := make(map[string]bool, len(want))
	for _, m := range want {
		keep[m.String()] = true
	}
	var stale []Manifest
	for _, m := range have {
		if !keep[m.String()] {
			stale = append(stale, m)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	stale = sortedByNameDesc(stale)
	names := make([]string, 0, len(stale))
	for _, m := range stale {
		names = append(names, m.String())
	}
	// Said before it happens, because this is the step that deletes things the
	// operator did not ask for in this run.
	d.log("pruning %d object(s) the spec no longer declares: %s", len(stale), strings.Join(names, ", "))

	// Every delete is ISSUED before any of them is waited on, and the order
	// they go out in carries no meaning.
	//
	// Teardown can reverse the author's order because the spec still lists
	// those objects. These are gone from it, so there is no declared order
	// left — and "no longer referenced by the spec" does not mean "references
	// nothing": two entries dropped in the same edit can reference each other,
	// a StorageConfig and the BackupSchedule using it being the obvious pair.
	// Deleting one at a time in an arbitrary order would then block on a live
	// reference for the whole timeout and fail the converge. Issuing them all
	// first lets the operator settle them in whatever order it needs.
	for _, m := range stale {
		if err := d.requestDelete(ctx, m); err != nil {
			return err
		}
	}
	for _, m := range stale {
		if err := d.awaitGone(ctx, m, d.universeTimeout); err != nil {
			return err
		}
	}
	return nil
}

// sortedByNameDesc orders objects by Kind/name, descending, for a stable log
// and a stable test. It is not a dependency order — nothing here has one.
func sortedByNameDesc(ms []Manifest) []Manifest {
	out := append([]Manifest(nil), ms...)
	sort.Slice(out, func(i, j int) bool { return out[i].String() > out[j].String() })
	return out
}

// operatorKinds asks the cluster which kinds exist, as the plural names
// kubectl addresses them by.
func (d *Deployer) operatorKinds(ctx context.Context) ([]string, error) {
	res, err := d.kubectlCluster(ctx, nil, "api-resources",
		"--api-group="+operatorAPIGroup, "--namespaced=true", "-o", "name")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("list the operator's kinds: %s", strings.TrimSpace(res.Stderr))
	}
	kinds := strings.Fields(res.Stdout)
	if len(kinds) == 0 {
		// Never "nothing to delete": the operator's CRDs are cluster-scoped
		// and this deployer installs them, so an empty answer means
		// discovery is broken. Treating it as an empty cluster would
		// silently stop reclaiming anything, forever.
		return nil, fmt.Errorf("the cluster reports no %s kinds, which cannot be right while the "+
			"operator is installed — refusing to decide what to delete from that",
			operatorAPIGroup)
	}
	return kinds, nil
}

// ownedObjects is every labelled operator CR in the namespace.
func (d *Deployer) ownedObjects(ctx context.Context, kinds []string) ([]Manifest, error) {
	res, err := d.kubectl(ctx, nil, "get", strings.Join(kinds, ","),
		"-l", ManagedByLabel+"="+ManagedByValue, "-o", "json")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("list this foundation's objects: %s", strings.TrimSpace(res.Stderr))
	}
	var list struct {
		Items []struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &list); err != nil {
		return nil, fmt.Errorf("parse this foundation's objects: %w", err)
	}
	out := make([]Manifest, 0, len(list.Items))
	for _, item := range list.Items {
		// Re-rendered rather than carried through, because deleteCR addresses
		// an object by its manifest and only needs enough to identify it.
		doc, err := marshal(map[string]any{
			"apiVersion": item.APIVersion,
			"kind":       item.Kind,
			"metadata": map[string]any{
				"name":      item.Metadata.Name,
				"namespace": d.spec.Namespace,
			},
		})
		if err != nil {
			return nil, err
		}
		out = append(out, Manifest{Kind: item.Kind, ObjectName: item.Metadata.Name, JSON: doc})
	}
	return out, nil
}
