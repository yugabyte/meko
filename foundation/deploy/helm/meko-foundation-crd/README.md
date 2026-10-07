# meko-foundation-crd

The `Foundation` CustomResourceDefinition, and nothing else.

```bash
helm install meko-foundation-crd ./meko-foundation-crd
```

**Install it once per cluster.** The CRD is cluster-scoped and shared by every
foundation on it, which is why it is a chart of its own rather than part of
[`meko-foundation`](../meko-foundation):

- Under `crds/` helm installs a CRD once and **never upgrades it**, so a status
  field or a printer column added later would never reach a cluster that already
  had the old one.
- As a template inside the per-foundation chart it would be a member of every
  release, and `helm uninstall` of **one** foundation would delete it for all of
  them.

Upgrading the schema is `helm upgrade` of this release.

`helm uninstall` deliberately **leaves the CRD behind**: it carries
`helm.sh/resource-policy: keep`, because deleting a CRD deletes every object of
that kind on the cluster — every foundation's input, at once. Removing it for
real is:

```bash
kubectl delete crd foundations.meko.yugabyte.com
```
