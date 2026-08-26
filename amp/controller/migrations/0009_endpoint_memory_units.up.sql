-- 0009: normalize stored endpoint memory reservations that carry no unit.
--
-- Kubernetes reads a unitless quantity as BYTES, so an endpoint stored with
-- memory "4" reserves four bytes, not 4Gi — and the API used to accept it
-- (meko#492). Every reader hands the stored value straight to the deployer
-- (create_endpoint, the add_branch_extensions redeploy, resize), so the bad value
-- keeps producing four-byte pods and wedges in-place resize in the kubelet
-- ("cannot decrease memory limits ... below current usage") until the value
-- itself changes. Repairing the row keeps one answer for what the endpoint
-- reserves, where substituting at each read would leave the readers disagreeing.
--
-- The pattern is a copy of `memoryQuantityRE` (internal/api/handlers.go), which
-- now rejects the same set at the door; TestMemoryQuantityPatternMatchesMigration
-- fails if the two drift. An empty memory is left alone — it is the "unset"
-- marker for endpoints predating resource tracking, and every reader already
-- substitutes the default for it.

UPDATE amp_registry.endpoints
   SET config = jsonb_set(config, '{memory}', '"4Gi"')
 WHERE config->>'memory' IS NOT NULL
   AND config->>'memory' <> ''
   AND config->>'memory' !~ '^[0-9]+(\.[0-9]+)?(Mi|Gi|Ti|Pi|Ei|M|G|T|P|E)$';
