import { useFeatureFlag } from "@/lib/useFeatureFlag";

/** Pre-GA gate for the Plans & billing page + sidebar entry. */
export function useBillingNavFlag(): boolean {
  return useFeatureFlag("showPaymentOptions");
}
