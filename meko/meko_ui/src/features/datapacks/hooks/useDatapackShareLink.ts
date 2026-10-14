// TanStack Query hooks for a datapack's share link: read it, change it, and copy
// its URL. Shared by the share page's link section and the details page's copy action.

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { shareLinkPath } from "@/app/routes/paths";
import { toast } from "@/hooks/use-toast";
import { copyToClipboard } from "@/lib/clipboard";
import { shareLinkUrl } from "@/lib/linkSharingFeature";
import {
  getShareLink,
  updateShareLink,
  type DatapackShareLink,
  type ShareLinkUpdate,
} from "@/lib/api/shareLinksApi";

export const shareLinkKey = (datapackId: string) => ["datapack-share-link", datapackId] as const;

/** The datapack's link (null when it has none). Owner or maintainer only: pass
 *  enabled=false for anyone else, since the API refuses them. */
export function useDatapackShareLink(datapackId: string, options?: { enabled?: boolean }) {
  return useQuery<DatapackShareLink | null>({
    queryKey: shareLinkKey(datapackId),
    queryFn: ({ signal }) => getShareLink(datapackId, { signal }),
    staleTime: 30_000,
    enabled: options?.enabled ?? true,
    retry: false,
  });
}

export function useUpdateDatapackShareLink(datapackId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: ShareLinkUpdate) => updateShareLink(datapackId, body),
    onSuccess: (link) => qc.setQueryData(shareLinkKey(datapackId), link),
  });
}

/** Copies the link's full URL and says whether it worked. */
export async function copyShareLinkUrl(token: string): Promise<void> {
  const copied = await copyToClipboard(shareLinkUrl(shareLinkPath(token)));
  toast(
    copied ? { title: "Link copied" } : { title: "Couldn't copy the link", variant: "destructive" },
  );
}
