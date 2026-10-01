-- 0010: caller-supplied key/value tags on clusters and endpoints, rendered as
-- Kubernetes labels on every object of an endpoint's release.
--
-- A cluster has no Kubernetes objects of its own, so its tags reach the cluster
-- only through its endpoints: an endpoint is labelled with `clusters.tags ||
-- endpoints.tags`, the endpoint winning a key both set. The merge happens at
-- render time rather than being copied onto the endpoint, so every redeploy
-- re-renders the same labels from the two rows.
--
-- Set at create and never updated: a pod-template label change restarts the
-- endpoint's pod.

ALTER TABLE amp_registry.clusters
    ADD COLUMN IF NOT EXISTS tags jsonb NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE amp_registry.endpoints
    ADD COLUMN IF NOT EXISTS tags jsonb NOT NULL DEFAULT '{}'::jsonb;
