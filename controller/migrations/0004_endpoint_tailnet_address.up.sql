-- 0004: a second, optional connection address for an endpoint — the Tailscale
-- (tailnet) URL, populated alongside the Kubernetes LoadBalancer address when
-- the endpoint Service is exposed on the tailnet. NULL/absent when the endpoint
-- is not tailnet-exposed; coalesced to '' on read like address.

ALTER TABLE endpoint ADD COLUMN tailnet_address text;
