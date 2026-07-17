package main

import (
	"fmt"
	"strconv"

	"github.com/yugabyte/yb-amp/test/functional/framework/env/registry"
)

// attachFromEntry maps a standing-registry entry to the env-provider name and the
// typed config overrides that make `run --env <id>` ATTACH to it instead of
// provisioning a fresh environment. Two attachable families:
//
//   - An amp control plane (provider "ampstack" / shape "dedicated_stack", or any
//     entry recording a controller_url/amp_controller_url or a kube
//     context+namespace) → the sharedk8s provider, which carves a project-per-test
//     out of the STANDING control plane. Connection info (controller_url or
//     context+namespace+controller_service/port, and any pinned pool) is read from
//     entry.Data and injected under env.sharedk8s.*.
//   - A universe target (entry recording attach_address, or attach_yba_url +
//     attach_yba_universe) → the universe provider in the matching attach mode.
//
// It errors when the entry's shape is not attachable — a bare VM host or a
// crash-recovery reap record is infrastructure to reap with `ampqa env rm`, not a
// target `run` can attach to. The returned overrides carry NO per-run identifier,
// so two sequential `run --env <id>` invocations against the same entry each get a
// fresh, collision-free project name from the run seed (sharedk8s names projects
// ampqa-p-<seed>-<n>).
func attachFromEntry(e registry.Entry) (provider string, overrides []string, err error) {
	d := e.Data

	// Amp control plane → sharedk8s.
	ctrlURL := firstNonEmpty(d["controller_url"], d["amp_controller_url"])
	kctx := d["context"]
	ns := firstNonEmpty(d["namespace"], d["amp_namespace"])
	if e.Provider == "ampstack" || e.Shape == "dedicated_stack" || ctrlURL != "" || (kctx != "" && ns != "") {
		if ctrlURL == "" && kctx == "" {
			return "", nil, fmt.Errorf("env %s (shape=%q) is an amp control plane but records neither a controller_url nor a kube context; re-attach with -o env.sharedk8s.context=<ctx>", e.ID, e.Shape)
		}
		ov := []string{}
		if ctrlURL != "" {
			ov = append(ov, "env.sharedk8s.controller_url="+ctrlURL)
		}
		if kctx != "" {
			ov = append(ov, "env.sharedk8s.context="+kctx)
		}
		if ns != "" {
			ov = append(ov, "env.sharedk8s.namespace="+ns)
		}
		if svc := d["controller_service"]; svc != "" {
			ov = append(ov, "env.sharedk8s.controller_service="+svc)
		}
		if p := d["controller_port"]; p != "" {
			if _, perr := strconv.Atoi(p); perr == nil {
				ov = append(ov, "env.sharedk8s.controller_port=int:"+p)
			}
		}
		if pid := d["pool_id"]; pid != "" {
			ov = append(ov, "env.sharedk8s.pool_id="+pid)
		}
		if pn := firstNonEmpty(d["pool"], d["pool_name"]); pn != "" {
			ov = append(ov, "env.sharedk8s.pool_name="+pn)
		}
		return "sharedk8s", ov, nil
	}

	// Universe target → universe attach.
	if addr := d["attach_address"]; addr != "" {
		return "universe", []string{
			"env.universe.mode=attach_address",
			"env.universe.addresses=list:" + addr,
		}, nil
	}
	if url := d["attach_yba_url"]; url != "" && d["attach_yba_universe"] != "" {
		ov := []string{
			"env.universe.mode=attach_yba",
			"env.universe.yba.url=" + url,
			"env.universe.yba.universe=" + d["attach_yba_universe"],
		}
		if c := d["attach_yba_customer"]; c != "" {
			ov = append(ov, "env.universe.yba.customer_uuid="+c)
		}
		if tok := d["attach_yba_token"]; tok != "" {
			ov = append(ov, "env.universe.yba.api_token="+tok)
		}
		return "universe", ov, nil
	}
	if e.Shape == "universe" {
		return "", nil, fmt.Errorf("env %s has shape universe but records no attach_address or attach_yba_url/attach_yba_universe to attach to", e.ID)
	}

	return "", nil, fmt.Errorf("env %s (shape=%q provider=%q) is not attachable by `run --env`: it records no amp controller (controller_url / context+namespace) or universe endpoint (attach_address / attach_yba_*). It is infrastructure to reap with `ampqa env rm`, not a target to run against", e.ID, e.Shape, e.Provider)
}

// firstNonEmpty returns the first non-empty argument, or "" if all are empty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
