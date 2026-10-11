package reap

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
	"github.com/yugabyte/meko/meko/test/functional/framework/kubemeta"
)

// MarkKept records ON THE CLUSTER that this plan's namespaces are held on
// purpose, so an out-of-band janitor stops reaping them on age.
//
// The registry entry alone cannot carry that: it is a file in the working
// directory, and a CI build's container is destroyed when the build ends. From
// then on the only record that a namespace was deliberate is the namespace
// itself, and a janitor that cannot tell "held" from "abandoned" must either
// reap a dev environment out from under someone or leave real leaks alone.
//
// until is the expiry (zero for no expiry, a standing environment, which the
// janitor reports instead of reaping). Every namespace is attempted even when one
// fails, and the errors are aggregated: a namespace that silently went unmarked
// is one a janitor will delete.
func MarkKept(ctx context.Context, p *Plan, until time.Time) error {
	if p == nil {
		return nil
	}
	labels, annotations := kubemeta.KeepMarkers(until)
	var errs []error
	for _, n := range p.Namespaces {
		if n.Namespace == "" {
			continue
		}
		targets := []NSReap{n}
		if n.Kind == KindDiscover {
			names, err := discoverNamespaces(ctx, n)
			if err != nil {
				errs = append(errs, fmt.Errorf("find the namespaces of %s: %w", n.Namespace, err))
				continue
			}
			targets = targets[:0]
			for _, name := range names {
				t := n
				t.Namespace = name
				targets = append(targets, t)
			}
		}
		for _, t := range targets {
			if err := markNamespace(ctx, t, labels, annotations); err != nil {
				errs = append(errs, fmt.Errorf("mark %s kept: %w", t.Namespace, err))
			}
		}
	}
	return errors.Join(errs...)
}

// discoverNamespaces is the seam unit tests replace. A discover entry names a
// universe, and YBA puts it in a namespace that contains the name.
var discoverNamespaces = func(ctx context.Context, n NSReap) ([]string, error) {
	kube := k8s.New(k8s.Config{Context: n.KubeContext}).WithRunner(k8s.KubeconfigRunner(n.Kubeconfig))
	out, err := kube.GetSelector(ctx, "namespaces", "", "{.items[*].metadata.name}")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, ns := range strings.Fields(out) {
		if strings.Contains(ns, n.Namespace) {
			names = append(names, ns)
		}
	}
	return names, nil
}

// markNamespace is the seam unit tests replace.
var markNamespace = func(ctx context.Context, n NSReap, labels, annotations map[string]string) error {
	// Same construction the reap steps use: the plan may name its own kubeconfig.
	kube := k8s.New(k8s.Config{Context: n.KubeContext}).WithRunner(k8s.KubeconfigRunner(n.Kubeconfig))
	for k, v := range labels {
		if err := kube.LabelNamespace(ctx, n.Namespace, k, v); err != nil {
			return err
		}
	}
	for k, v := range annotations {
		if err := kube.AnnotateNamespace(ctx, n.Namespace, k, v); err != nil {
			return err
		}
	}
	return nil
}
