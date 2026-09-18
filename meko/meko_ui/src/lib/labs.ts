// Feature gate for Discovery Labs — the left-nav entry and every /labs route.
//
// A RUNTIME flag served via /runtime-config.js, so it flips per deployment
// without a rebuild, the same way MEKO_ENABLE_CONTEXTUAL_PROMINENCE_SEARCH and
// MEKO_ENABLE_CONVERSATION_SEARCH do. Off unless explicitly enabled: a
// deployment that has not set it gets no Labs, which is what makes turning it
// on a deliberate act per environment rather than something prod inherits.
//
// Unlike the Admin console there is no second per-browser opt-in — Labs is a
// product surface, not an internal one, so where the flag is on it is on for
// every user of that deployment.

import { ROUTES } from "@/app/routes/paths";
import { readRuntimeEnv } from "@/lib/runtimeEnv";

/** Same spelling the BE and MCP server accept, so one value means the same
 *  thing in every service's ConfigMap. */
export function isLabsEnabled(): boolean {
  return ["1", "true", "yes"].includes(
    (readRuntimeEnv("MEKO_ENABLE_LABS") ?? "").trim().toLowerCase(),
  );
}

/** True for every Labs URL, the lab runner included -- `/labs/:labId/run` is
 *  itself under `/labs`, so one prefix covers the list, the detail page and
 *  the runner. Kept separate from the flag so the routing decision is
 *  testable without a rendered tree. */
export function isLabsPath(pathname: string): boolean {
  return pathname === ROUTES.labs || pathname.startsWith(`${ROUTES.labs}/`);
}
