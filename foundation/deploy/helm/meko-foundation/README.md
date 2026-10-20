# meko-foundation

One pod that reads a `Foundation` object and converges a YugabyteDB universe
onto it, through the YugabyteDB Anywhere Kubernetes operator.

```bash
helm registry login quay.io            # the charts are private, like the images

# once per cluster — the CRD is cluster-scoped and shared
helm install meko-foundation-crd oci://quay.io/yugabyte/charts/meko-foundation-crd

# once per foundation
helm install fdn oci://quay.io/yugabyte/charts/meko-foundation \
  --namespace amp-foundation --create-namespace
kubectl -n amp-foundation apply -f foundation-cr.yaml
```

No `--set image.tag`: the chart's `appVersion` names the image built in the same
CI run, so a bare install runs a matching one. Installing from a checkout
(`helm install fdn ./meko-foundation`) is a developer path and resolves
`:latest` instead — see [Versions and tags](#versions-and-tags).

The chart does **not** create the `Foundation`. That object is your input, and a
chart that owned it would revert your edits on the next `helm upgrade`. Start
from [`examples/foundation-cr.yaml`](../../../examples/foundation-cr.yaml); the
object's name must match `foundation.name`.

That example is complete rather than minimal, and the comments mark every value
you must replace — the namespace, the registries and pull secret, the yugaware
password, the region and zone codes, the storage classes and the CA. Three of
its settings are **create-time only**: both encryption flags and the MAGE
gflags. A universe built without them cannot be edited into having them, only
rebuilt, so get those right before the first apply rather than after.

Nothing in the foundation creates the `yb_storage` database the amp controller
opens at startup. **YBA** does, from the global runtime flag
`yb.universe.create_yb_storage_db` that the example sets — but only as a subtask
of universe **create**, and only on a build carrying it (yugabyte-db `2026.1`
`eab8d8b1d`, 2026-10-05; chart `2026.1.3.0-b48` predates it). On anything older,
or for a universe that already exists, create it yourself before the consumer
rolls out:

```bash
CREATE DATABASE yb_storage;
```

[`ARCHITECTURE.md`](../../../ARCHITECTURE.md) is how the thing works. This file
is what you need before installing it.

## What this pod can do to your cluster

**With `rbac.clusterScoped` left on — the default — this ServiceAccount is
effectively cluster-admin.** It holds `escalate` on `clusterroles` and `create`
on `clusterrolebindings`, which together let it grant itself anything. Nothing
in the chart or the binary stops that; it is what the grant means.

That is not an oversight and it cannot be narrowed away. Installing the yugaware
chart with its Kubernetes operator enabled means creating cluster-scoped RBAC
for that operator, and Kubernetes refuses to let anything create RBAC it does
not already hold. The pod also applies the operator's own
CustomResourceDefinitions, which are cluster-scoped too.

In full, what the chart grants:

| Scope | Objects | Verbs | Why |
|---|---|---|---|
| This namespace (and `rbac.extraNamespaces`) | everything | everything | The pod runs `helm upgrade --install` of the yugaware chart, whose contents that chart decides and changes between versions — and the spec picks the version. A list of kinds here would go stale on a chart bump. |
| Cluster | `customresourcedefinitions` | get, list, watch, create, update, patch | The operator's CRD bundle, applied at the start of every converge. **No delete** — they are shared, and nothing in this binary ever deletes one. |
| Cluster | `namespaces` | get, list, create, patch | Preflight says which namespaces exist when the spec's is missing; the converge creates and labels its own. |
| Cluster | `storageclasses` | get, list | Preflight reads `volumeBindingMode`. |
| Cluster | `clusterroles` | get, list, watch, create, update, patch, delete, **escalate**, **bind** | The yugaware release's own cluster RBAC. |
| Cluster | `clusterrolebindings` | get, list, watch, create, update, patch, delete | The same. |

### Running it with less

Set `rbac.clusterScoped=false` and the pod keeps only the namespaced wildcard.
It is then **not** cluster-admin — but it also cannot install the operator's
CRDs or the yugaware chart's cluster RBAC, so a cluster admin has to do both
before any converge will finish. A converge that hits a missing grant fails with
a `forbidden` naming exactly what it needed, which is a legible failure rather
than a mysterious one.

There is no supported way to run it with less than the namespaced wildcard: the
pod installs a third-party chart into that namespace.

## Deleting things

Deleting the `Foundation` **leaves the universe running** by default. That is
recoverable — applying the same spec again converges back onto it.

`spec.deletionPolicy: Delete` makes deletion tear the universe down, and it is
the setting that puts a finalizer on the object. Know two things before using it:

- **Delete the `Foundation` before the namespace, not with it.** Only this pod
  can clear that finalizer, and `kubectl delete namespace` removes the pod at
  the same moment as the object. The teardown gives up after an hour and
  releases the object anyway, which keeps the namespace from being stuck in
  `Terminating` for good, but what it leaves behind is then yours to remove.
- **Flipping the policy back to `Retain` drops the finalizer** on the next pass.
  That is the escape hatch for an object whose teardown cannot finish.
- **Changing `Retain` to `Delete` takes a pass to take effect.** The finalizer is
  added by the loop, not by the API server, so an object deleted in the seconds
  after the edit has none yet — it simply goes, and the universe stays. Wait for
  it before deleting:

  ```bash
  kubectl -n amp-foundation patch foundation foundation --type=merge \
    -p '{"spec":{"deletionPolicy":"Delete"}}'
  kubectl -n amp-foundation get foundation foundation \
    -o jsonpath='{.metadata.finalizers}'      # until it lists .../teardown
  kubectl -n amp-foundation delete foundation foundation
  ```

**Never delete the namespace first**, under either policy. The operator's own
CRs — `YBUniverse`, `YBProvider` and the rest — carry its finalizer, the
operator is deleted with the namespace, and the namespace then stays in
`Terminating` for good; deleting it again does nothing. Delete the `Foundation`
with `deletionPolicy: Delete` first, or the `operator.yugabyte.io` CRs yourself
(universe first, provider last), and the namespace after.

A namespace already stuck that way reports
`finalizer.k8soperator.yugabyte.com` (or `meko.yugabyte.com/teardown`, the
`Foundation`'s) under `NamespaceFinalizersRemaining`. Nothing is left that can
clear them, so remove them by hand. **First check where the universe ran**: if
a provider zone's `kubeNamespace` put it in another namespace, its pods, PVCs
and Services are still running there and nothing will tear them down once the
finalizers go — delete them yourself.

```bash
NS=amp-foundation
for kind in foundations.meko.yugabyte.com \
    $(kubectl api-resources --api-group=operator.yugabyte.io --namespaced -o name); do
  for obj in $(kubectl -n "$NS" get "$kind" -o name); do
    kubectl -n "$NS" patch "$obj" --type merge -p '{"metadata":{"finalizers":null}}'
  done
done
```

`helm uninstall` of this chart removes the controller and nothing else: the
universe, the yugaware release and the operator's CRDs all outlive it. Removing
the CRD chart does not delete the CRD either — it carries
`helm.sh/resource-policy: keep`, because deleting a CRD deletes every object of
that kind on the cluster.

## Versions and tags

The operator's version is **derived from the commit, not declared** —
`MAJOR.YYMMDD.PATCH` from [`scripts/version.sh`](../../../scripts/version.sh),
where the patch counts the foundation commits that landed earlier that UTC day.
There is no VERSION file to bump: amp has that scheme and its file has two
commits in its whole history, so a number that depends on somebody remembering
to move it goes stale — and a stale number that names an image tag is the
failure this avoids.

One run of `foundation-image.yml` writes all of these from one commit:

| artifact | value | |
|---|---|---|
| image `0.261014.3` | **pin this** | immutable; only the publishing run writes it |
| image `0.261014.3-<sha>` | the same build, carrying the commit | immutable |
| image `custom-<sha>` | every build, including PR lanes | immutable |
| image `latest` | what a checkout install resolves to | floating, main only |
| image `yba-2026.1.3.0-b21` | the newest build targeting that YBA | floating — **do not pin** |

Only `.github/workflows/foundation-image.yml` passes `build-image.sh --release`,
which is what makes the version tags its alone to write. Every other caller —
the GKE lane builds this image on each `/ci amp_deploy_gke_foundation` run, with
push credentials — gets `custom-<sha>` only. Ungated, those runs overwrote the
version a published chart names through `appVersion`, so installing the chart
ran a PR branch's code.
| chart `meko-foundation` | `version` and `appVersion` both `0.261014.3` | the version IS the OCI tag |
| chart `meko-foundation-crd` | its own `version`, bumped with the schema | cluster-scoped and shared, so a bump means the schema moved |

```bash
helm show chart oci://quay.io/yugabyte/charts/meko-foundation --version 0.261014.3
helm install fdn oci://quay.io/yugabyte/charts/meko-foundation                     # newest
helm install fdn oci://quay.io/yugabyte/charts/meko-foundation --version 0.261014.3 # pinned
```

### Which YBA is it qualified with

`helm show chart` prints it first:

```
annotations:
  meko.yugabyte.com/tested-with-yba: 2026.1.3.0-b21
```

**The build this release targets, not a constraint.** The operator installs
whatever `chartVersion` the `Foundation` names — the YBA version is an input to
the CR, and two foundations on one cluster can run different ones from the same
pod image.

Read it precisely: the annotation is guaranteed to equal the shipped example's
`chartVersion` and `ybSoftwareVersion`, which a test enforces. It is **not** a
record that a universe came up on that build — `/ci amp_deploy_gke_foundation`
is directive-gated and nightly, and publishing does not wait on it. For whether
it actually worked, read that lane's result.

It is an annotation rather than the chart version because a Helm version must be
SemVer 2, and `2026.1.3.0-b21` has four numeric parts — helm rejects it
(`Error: invalid semantic version`). `2026.1.3-b21` does parse, but `-b21` makes
it a *prerelease*, which helm skips without `--devel`, so a bare `helm install`
would resolve nothing at all. An annotation has no format constraint.

## Values worth knowing

| Key | Default | |
|---|---|---|
| `image.tag` | the chart's `appVersion` | Unset is right for a published chart — its `appVersion` is the image from the same run. Set it only to override. See [Versions and tags](#versions-and-tags). |
| `foundation.name` | `foundation` | The object this pod watches. One pod per Foundation: two against one object would race each other's helm releases. |
| `reconcile.poll` | 30s | How often the object is re-read. Doubles as the debounce. |
| `reconcile.resync` | off | Re-converge a settled foundation on a timer. Off because a converge runs `helm upgrade`, which would cut a release revision every interval for nothing. |
| `terminationGracePeriodSeconds` | 600 | On SIGTERM the loop gives up what it can and lets a step that *writes* finish, because `helm upgrade` killed partway leaves the release holding a lock that later upgrades refuse. A cold yugaware install can exceed this, and an eviction or node scale-down ignores it entirely — so the next start detects a wedged release and clears it: rolled back to the newest deployed revision, or uninstalled only if none ever deployed. Raise it above your yba timeout to make that recovery rarer. |
| `rbac.extraNamespaces` | `[]` | Needed only when a provider zone's `kubeNamespace` puts the universe in another namespace — the Service lookup, the certificate check and the pod dump all run there. |
