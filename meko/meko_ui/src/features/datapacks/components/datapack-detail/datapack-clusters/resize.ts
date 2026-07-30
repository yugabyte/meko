// Pure helpers for the compute resize flow: the default reservation shown as a
// placeholder, and the mapping from an AMP resize outcome to toast copy.

import type { AmpResizeOutcome } from "@/lib/api/ampApi";

// Default (2 cores / 4Gi) shown as the resize placeholder when the controller
// hasn't reported an endpoint's current reservation.
export const DEFAULT_CPU = "2";
export const DEFAULT_MEMORY = "4Gi";

// Outcome → toast copy, mirroring AMP's per-outcome messaging. Only "infeasible"
// is surfaced as an error (destructive); the rest are informational.
export function resizeToast(
  outcome: AmpResizeOutcome,
  size: string,
): { title: string; variant?: "destructive" } {
  switch (outcome) {
    case "completed":
      return { title: `Resized to ${size}` };
    case "pending":
      return { title: `Resize to ${size} scheduled (applies on the next restart)` };
    case "unsupported":
      return { title: `Resized to ${size} via a restart (live resize unsupported)` };
    case "infeasible":
      return { title: `Resize to ${size} isn't feasible on the current node`, variant: "destructive" };
  }
}
