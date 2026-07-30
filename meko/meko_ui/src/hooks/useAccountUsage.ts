// useAccountUsage — react-query wrapper around GET /auth/me/usage.
//
// Returns the four raw counters (datapacks / KB storage bytes /
// WorkbenchLM conversations / WorkbenchLM token burn) for the
// signed-in user. The thresholds live in `lib/freeTier.ts`; this hook
// is the data source.
//
// `staleTime: 60s` is a deliberate trade-off:
//   - Long enough that bouncing between gated surfaces (Datapacks
//     overview → Connect → WorkbenchLM) doesn't re-fetch four times
//     in 10 seconds.
//   - Mutations that consume the budget (create
//     datapack, upload KB doc, send a WB message) should still call
//     `queryClient.invalidateQueries({ queryKey: [ACCOUNT_USAGE_QUERY_KEY] })`
//     so the gates flip immediately rather than after staleness expires.

import { useQuery } from "@tanstack/react-query";
import { getCurrentUserUsage, type AccountUsage } from "@/lib/api/mekoApi";
import { FREE_TIER_QUOTA_ENABLED } from "@/lib/freeTier";

export const ACCOUNT_USAGE_QUERY_KEY = "account-usage";

export function useAccountUsage(options?: { enabled?: boolean }) {
  // When the master `MEKO_FREE_TIER_QUOTA_ENABLED` flag is off, skip
  // the /auth/me/usage fetch entirely — no consumer applies caps in
  // that mode, so the network request is wasted load on api_server.
  const enabled = (options?.enabled ?? true) && FREE_TIER_QUOTA_ENABLED;
  return useQuery<AccountUsage>({
    queryKey: [ACCOUNT_USAGE_QUERY_KEY],
    queryFn: ({ signal }) => getCurrentUserUsage({ signal }),
    staleTime: 60_000,
    enabled,
  });
}
