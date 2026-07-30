// Feature gate for the in-app Admin console (Foundation, Metrics, Executive
// Dashboard, Users & tiers, AMP registration) — the meko_admin surfaces
// integrated into the main app.
//
// TWO gates must both be true for the Admin tab to appear:
//  1. SHOW_ADMIN_VIEW — a RUNTIME flag (served via /runtime-config.js), set
//     per-deployment without a rebuild. Off unless explicitly "true".
//  2. localStorage `showMekoAdminView` === "true" — a per-browser opt-in, so
//     that even on an environment where the flag is on (e.g. dev), the Admin
//     view is NOT exposed to every user. A developer self-serves with:
//         localStorage.setItem("showMekoAdminView", "true")
//
// This is the interim gate until a per-user admin ROLE lands; then the default
// flips ON and we additionally require `user.is_admin`.

import { readRuntimeEnv } from "@/lib/runtimeEnv";
import { getLocalItem } from "@/lib/browserStorage";

/** localStorage key a developer sets to reveal the Admin tab where the runtime
 *  flag is enabled. Exported so it stays a single source of truth. */
export const ADMIN_VIEW_LOCAL_KEY = "showMekoAdminView";

export function isAdminViewEnabled(): boolean {
  const flagOn = (readRuntimeEnv("SHOW_ADMIN_VIEW") ?? "").trim().toLowerCase() === "true";
  if (!flagOn) return false;
  return (getLocalItem(ADMIN_VIEW_LOCAL_KEY) ?? "").trim().toLowerCase() === "true";
}
