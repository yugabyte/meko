// Fetches datapack members and pending invites for the owner share UI (GET outgoing-shares).
// Exposes loading/error state and refetch; use silent refetch after mutations to avoid full-page loading flashes.
import { useCallback, useEffect, useState } from "react";
import {
  getApiErrorMessage,
  listDatapackOutgoingShares,
  type DatapackOutgoingSharesResponse,
} from "@/lib/mekoApi";

export function useDatapackOutgoingShares(datapackName: string) {
  const [data, setData] = useState<DatapackOutgoingSharesResponse | null>(null);
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
      const next = await listDatapackOutgoingShares(datapackName);
      setData(next);
    } catch (err) {
      setData(null);
      setError(getApiErrorMessage(err, "Could not load sharing data."));
    } finally {
      if (!silent) {
        setLoading(false);
      }
    }
  }, [datapackName]);

  useEffect(() => {
    void refetch();
  }, [refetch]);

  return { data, loading, error, refetch };
}
