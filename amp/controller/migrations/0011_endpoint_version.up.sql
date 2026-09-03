-- 0011: record the version each endpoint runs, and the one it should run.
--
-- Until now a version is deployer configuration (`--endpoint-image-tag`, stamped
-- into every release), so the controller cannot say what any one endpoint runs and
-- nothing can ask for a different one. The column makes it a property of the
-- endpoint: an opaque string the deployer knows how to run — today the endpoint
-- image tag verbatim, which is why it is `text` and not a structured type.
--
-- `version` is what the endpoint is recorded as RUNNING, written after a deploy
-- reports back; every controller-driven redeploy renders from it, so an endpoint
-- stays where it is instead of reverting to whatever the controller is configured
-- with. `target_version` is what it SHOULD run, written before the deploy. The two
-- differing is an upgrade that has not converged — which is what makes "did this
-- endpoint actually upgrade?" answerable from the row rather than inferred.
--
-- NULL means unknown, which is what every endpoint predating this has. Unknown is
-- upgradeable — there is nothing to compare against — and the create and upgrade
-- steps write what the deployer reports, so one pass moves the fleet from unknown
-- to known. No backfill: guessing that an existing endpoint runs the controller's
-- *currently configured* tag is exactly the assumption that breaks after the first
-- controller upgrade.

ALTER TABLE amp_registry.endpoints
    ADD COLUMN IF NOT EXISTS version text,
    ADD COLUMN IF NOT EXISTS target_version text;
