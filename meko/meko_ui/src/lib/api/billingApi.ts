// Billing API surface — Plans & billing page and the Stripe redirects
// hang off these three endpoints. React-query hooks in features/billing
// wrap them; nothing else should call them directly.

import { request } from "@/lib/api/mekoApi";

/**
 * GET /billing/summary — current-plan card data: renews date,
 * cancel-at-period-end flag, and the card brand + last4 of the
 * default payment method on file. All fields are optional; the FE
 * shows em-dashes for any blank.
 */
export type BillingSummary = {
  plan_id?: string;
  status?: string;
  current_period_end?: string;
  cancel_at_period_end?: boolean;
  card_brand?: string;
  card_last4?: string;
};

export async function getBillingSummary(
  options: { signal?: AbortSignal } = {},
): Promise<BillingSummary> {
  return request<BillingSummary>("/billing/summary", "GET", undefined, options);
}

export type CheckoutSessionResponse = { url: string };
export type PortalSessionResponse = { url: string };
export type CheckoutPlanId = "starter" | "growth" | "scale";

export async function createBillingCheckoutSession(
  planId: CheckoutPlanId,
  options: { signal?: AbortSignal } = {},
): Promise<CheckoutSessionResponse> {
  return request<CheckoutSessionResponse>(
    "/billing/checkout-session",
    "POST",
    { plan_id: planId },
    options,
  );
}

export async function createBillingPortalSession(
  options: { signal?: AbortSignal } = {},
): Promise<PortalSessionResponse> {
  return request<PortalSessionResponse>(
    "/billing/portal-session",
    "POST",
    undefined,
    options,
  );
}

/** Shared cache key prefix for all billing-related queries. */
export const BILLING_QUERY_KEYS = {
  summary: ["billing", "summary"] as const,
};
