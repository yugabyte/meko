import { useMutation } from "@tanstack/react-query";
import { toast } from "@/hooks/use-toast";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import {
  createBillingCheckoutSession,
  createBillingPortalSession,
  type CheckoutPlanId,
} from "@/lib/api/billingApi";
import type { PlanId } from "@/features/billing/constants/plans";

/**
 * 404 from /billing/* almost always means the BE didn't register the
 * billing routes -- this happens whenever any of STRIPE_SECRET_KEY,
 * STRIPE_WEBHOOK_SECRET, or STRIPE_PRICE_* env vars are missing on the
 * api_server. We surface that specific cause in the toast so a local
 * dev doesn't have to dig through server logs to figure it out.
 */
function billingErrorMessage(err: unknown, defaultMsg: string): string {
  const status = (err as { response?: { status?: number } })?.response?.status;
  if (status === 404) {
    return "Billing isn't configured on this server";
  }
  return getApiErrorMessage(err, defaultMsg);
}

/**
 * React-query mutations for the two Stripe redirect flows. The hook
 * exposes `busy` (true while either mutation is pending) and two
 * imperative trigger functions used by the plan-picker / current-plan
 * cards. On success we full-page navigate to the Stripe-hosted URL;
 * on failure we surface an actionable toast.
 *
 * Why mutations and not raw fetch: react-query gives us a shared
 * pending flag, automatic abort on unmount, and a single error path,
 * which together replace the busy useState + try/catch boilerplate.
 */
export function useBillingActions() {
  const checkout = useMutation({
    mutationFn: (planId: CheckoutPlanId) => createBillingCheckoutSession(planId),
    onSuccess: ({ url }) => {
      window.location.assign(url);
    },
    onError: (err) => {
      toast({
        title: "Couldn't start checkout",
        description: billingErrorMessage(err, "Try again in a moment."),
      });
    },
  });

  const portal = useMutation({
    mutationFn: () => createBillingPortalSession(),
    onSuccess: ({ url }) => {
      window.location.assign(url);
    },
    onError: (err) => {
      toast({
        title: "Couldn't open billing portal",
        description: billingErrorMessage(err, "Upgrade to a paid plan first"),
      });
    },
  });

  const busy = checkout.isPending || portal.isPending;

  const openCheckout = (planId: PlanId) => {
    if (busy) return;
    if (planId === "free" || planId === "enterprise") return;
    checkout.mutate(planId);
  };

  const openPortal = () => {
    if (busy) return;
    portal.mutate();
  };

  return { busy, openCheckout, openPortal };
}
