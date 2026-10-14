// Fetches datapack members (GET member-list) and pending invites (GET outgoing-shares) for the share UI.
// Contributors get member-list only; viewers are refused both, reported as membersHidden.
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
  // True when the member list was refused (403): the caller is a viewer and
  // has nothing to see or manage on the share page.
  const [membersHidden, setMembersHidden] = useState(false);

  const refetch = useCallback(
    async (options: { silent?: boolean } = {}) => {
      const { silent = false } = options;
      // Initial load and explicit retry show the loading state; post-invite refresh does not.
      if (!silent) {
        setLoading(true);
      }
      setError(null);
      try {
        let members: DatapackShareListsResponse["members"] = [];
        let hidden = false;
        try {
          members = (await listDatapackMembers(datapackId, {})).members ?? [];
        } catch (err) {
          if (!(isMekoApiRequestError(err) && err.httpStatus === 403)) {
            throw err;
          }
          hidden = true;
        }
        setMembersHidden(hidden);
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
          members,
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
    },
    [datapackId],
  );

  useEffect(() => {
    void refetch();
  }, [refetch]);

  return { data, loading, error, membersHidden, refetch };
}
