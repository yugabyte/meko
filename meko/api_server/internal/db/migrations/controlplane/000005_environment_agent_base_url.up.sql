/*
 * The YBM pass-through base an environment's agent dials (control-plane-mode
 * migration set).
 *
 * Registration takes it as a request field and it used to be forgotten once the
 * install hint was rendered, which made the hint reproducible only in that one
 * response. GET /environments/{id} renders the same hint, so the value has to
 * outlive registration: without it a YBM-registered environment's hint would fall
 * back to the address the GET arrived on and print a URL the agent cannot reach.
 *
 * Nullable, and never rewritten: an environment registered before this (or
 * directly, with no YBM in front) keeps the fallback.
 */
ALTER TABLE meko_system.environments
    ADD COLUMN IF NOT EXISTS agent_base_url TEXT;
