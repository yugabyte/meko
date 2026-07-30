import { useQuery } from "@tanstack/react-query";
import {
  BILLING_QUERY_KEYS,
  getBillingSummary,
  type BillingSummary,
} from "@/lib/api/billingApi";

/**
 * Pulls /billing/summary (renews date + card on file) via react-query.
 * Free-tier users get an empty object back; the UI renders em-dashes
 * for any missing field. Soft-fails to null so a Stripe-side blip
 * still leaves the page rendering with placeholders.
 */
export type BillingSummaryState = {
  summary: BillingSummary | null;
  loading: boolean;
};

export function useBillingSummary(): BillingSummaryState {
  const { data, isLoading } = useQuery<BillingSummary>({
    queryKey: BILLING_QUERY_KEYS.summary,
    queryFn: ({ signal }) => getBillingSummary({ signal }),
    // Plan + card data changes only on subscription events; a short
    // staleTime avoids re-fetching on every BillingPage mount.
    staleTime: 30 * 1000,
    retry: false,
  });

  return {
    summary: data ?? null,
    loading: isLoading,
  };
}
