// Fetches datapack members (GET member-list) and pending invites (GET outgoing-shares) for the share UI.
// Viewers/contributors get member-list only; pending list returns 403 and is treated as empty.
import { useCallback, useEffect, useState } from "react";
import {
  getApiErrorMessage,
  isMekoApiRequestError,
  listDatapackMembers,
  listDatapackOutgoingShares,
  type DatapackShareListsResponse,
} from "@/lib/api/mekoApi";

export function useDatapackOutgoingShares(datapackId: string) {
  const [data, setData] = useState<DatapackShareListsResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const refetch = useCallback(async (options: { silent?: boolean } = {}) => {
    const { silent = false } = options;
    // Initial load and explicit retry show the loading state; post-invite refresh does not.
    if (!silent) {
      setLoading(true);
    }
    setError(null);
    try {
      const membersRes = await listDatapackMembers(datapackId, {});
      let pendingInvites: DatapackShareListsResponse["pending_invites"] = [];
      try {
        const out = await listDatapackOutgoingShares(datapackId, {});
        pendingInvites = out.pending_invites ?? [];
      } catch (err) {
        if (isMekoApiRequestError(err) && err.httpStatus === 403) {
          pendingInvites = [];
        } else {
          throw err;
        }
      }
      setData({
        members: membersRes.members ?? [],
        pending_invites: pendingInvites,
      });
    } catch (err) {
      setData(null);
      setError(getApiErrorMessage(err, "Could not load sharing data."));
    } finally {
      if (!silent) {
        setLoading(false);
      }
    }
  }, [datapackId]);

  useEffect(() => {
    void refetch();
  }, [refetch]);

  return { data, loading, error, refetch };
}
