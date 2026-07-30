import { useEffect, useState } from "react";
import { getCurrentUser } from "@/lib/api/mekoApi";
import { getPlan, type Plan, type PlanId } from "@/features/billing/constants/plans";

/**
 * Maps the BE's current `user_type` enum (free | pro | enterprise) to the
 * 5-tier plan catalog the UI displays. Until the BE enum is widened to
 * match, "pro" parks on "growth" (the closest paid mid-tier) and any
 * unknown value falls back to "free".
 */
function planIdFromUserType(userType: string | undefined): PlanId {
  switch ((userType ?? "").toLowerCase()) {
    case "starter":
      return "starter";
    case "growth":
    case "pro":
      return "growth";
    case "scale":
      return "scale";
    case "enterprise":
      return "enterprise";
    default:
      return "free";
  }
}

export type CurrentPlanState = {
  plan: Plan;
  planId: PlanId;
  loading: boolean;
};

/**
 * Resolves the signed-in user's billing plan via `/auth/me`. Defaults to
 * the Free plan while the request is in flight or if the BE doesn't yet
 * return `user_type` — the billing page renders the same shell either way,
 * just with different denominators in the usage bars.
 */
export function useCurrentPlan(): CurrentPlanState {
  const [planId, setPlanId] = useState<PlanId>("free");
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    const ac = new AbortController();
    setLoading(true);
    void getCurrentUser({ signal: ac.signal })
      .then((u) => {
        if (ac.signal.aborted) return;
        setPlanId(planIdFromUserType(u.user_type));
      })
      .catch(() => {
        if (ac.signal.aborted) return;
        setPlanId("free");
      })
      .finally(() => {
        if (ac.signal.aborted) return;
        setLoading(false);
      });
    return () => ac.abort();
  }, []);

  return { plan: getPlan(planId), planId, loading };
}
