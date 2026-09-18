// useTokenSavings — react-query wrappers around the token-savings endpoints.
// Feeds the TOKEN BURN efficiency tile in the datapack detail view.
// Lifetime counters, so a longish staleTime is fine.

import { useQuery } from "@tanstack/react-query";
import {
  getDatapackTokenSavings,
  type TokenSavingsSummary,
} from "@/lib/api/mekoApi";

export const DATAPACK_TOKEN_SAVINGS_QUERY_KEY = "datapack-token-savings";

const STALE_TIME_MS = 60_000;

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
