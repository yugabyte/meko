// useTokenSavings — react-query wrappers around the token-savings endpoints
// (GET /me/token-savings and GET /datapacks/:id/token-savings). Feeds the
// TOKEN BURN efficiency tile. Lifetime counters, so a longish staleTime is fine.

import { useQuery } from "@tanstack/react-query";
import {
  getAccountTokenSavings,
  getDatapackTokenSavings,
  type TokenSavingsSummary,
} from "@/lib/api/mekoApi";

export const ACCOUNT_TOKEN_SAVINGS_QUERY_KEY = "account-token-savings";
export const DATAPACK_TOKEN_SAVINGS_QUERY_KEY = "datapack-token-savings";

const STALE_TIME_MS = 60_000;

// Account-wide savings across every datapack the caller can access.
export function useAccountTokenSavings(options?: { enabled?: boolean }) {
  return useQuery<TokenSavingsSummary>({
    queryKey: [ACCOUNT_TOKEN_SAVINGS_QUERY_KEY],
    queryFn: ({ signal }) => getAccountTokenSavings({ signal }),
    staleTime: STALE_TIME_MS,
    enabled: options?.enabled ?? true,
  });
}

// Savings for a single datapack (by UUID). Disabled until an id is available.
export function useDatapackTokenSavings(datapackId: string | null | undefined) {
  return useQuery<TokenSavingsSummary>({
    queryKey: [DATAPACK_TOKEN_SAVINGS_QUERY_KEY, datapackId],
    queryFn: ({ signal }) => {
      if (!datapackId) {
        throw new Error("datapackId is required to fetch token savings");
      }
      return getDatapackTokenSavings(datapackId, { signal });
    },
    staleTime: STALE_TIME_MS,
    enabled: Boolean(datapackId),
  });
}
