package main

import (
	"fmt"

	"github.com/yugabyte/meko/meko/test/functional/framework/env/registry"
)

// attachFromEntry maps a standing-registry entry to the env-provider name and the
// typed config overrides that make `run --env <id>` attach to it instead of
// provisioning a fresh stack. An entry recording a kube context and a namespace
// is a standing stack and resolves to the k8s provider. Anything else is
// infrastructure to reap with `mekoqa env rm`, not a target `run` can attach to.
//
// The overrides carry no per-run identifier, so two sequential
// `run --env <id>` invocations never collide.
func attachFromEntry(e registry.Entry) (provider string, overrides []string, err error) {
	d := e.Data
	kctx, ns := d["context"], d["namespace"]
	if kctx != "" && ns != "" {
		return "k8s", []string{"env.k8s.context=" + kctx, "env.k8s.namespace=" + ns}, nil
	}
	return "", nil, fmt.Errorf("env %s (shape=%q provider=%q) is not attachable by `run --env`: it records no kube context and namespace. It is infrastructure to reap with `mekoqa env rm`, not a target to run against", e.ID, e.Shape, e.Provider)
}
