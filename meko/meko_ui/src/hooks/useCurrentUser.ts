// useCurrentUser — react-query wrapper around GET /auth/me.
//
// The signed-in user's identity is stable for the whole session, so this
// is cached with `staleTime: Infinity`: the first caller fetches `/me`
// once and every other subscriber (across the app, since the key is a
// constant) reads the same cache entry. That keeps `/me` out of polling
// loops — e.g. the WorkbenchLM live-observation panel used to re-hit
// `/me` on every memories poll because it resolved the account id inside
// the polled fetch.

import { useQuery } from "@tanstack/react-query";
import { getCurrentUser, type CurrentUser } from "@/lib/api/mekoApi";

export const CURRENT_USER_QUERY_KEY = "current-user";

export function useCurrentUser(options?: { enabled?: boolean }) {
  return useQuery<CurrentUser>({
    queryKey: [CURRENT_USER_QUERY_KEY],
    queryFn: ({ signal }) => getCurrentUser({ signal }),
    // Identity doesn't change within a session → never auto-refetch.
    staleTime: Infinity,
    // Lets callers gate the fetch (e.g. a header that only shows the
    // avatar on some routes). Disabled observers don't fetch but still
    // read the shared cache if another caller already populated it.
    enabled: options?.enabled ?? true,
  });
}
