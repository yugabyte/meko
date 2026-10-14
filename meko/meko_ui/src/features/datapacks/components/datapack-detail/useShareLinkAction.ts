// What the details page's share-link action does: copy an active link, or open
// the share page at its Link sharing card to create or turn one on. Owner and
// maintainer only.

import { useNavigate } from "react-router-dom";
import { datapackSharePath } from "@/app/routes/paths";
import { SHARE_LINK_STATUS_ACTIVE } from "@/lib/api/shareLinksApi";
import { SHARE_PAGE_HIGHLIGHT_LINK, SHARE_PAGE_HIGHLIGHT_PARAM } from "@/lib/linkSharingFeature";
import {
  copyShareLinkUrl,
  useDatapackShareLink,
} from "@/features/datapacks/hooks/useDatapackShareLink";

export function useShareLinkAction(datapackId: string, enabled: boolean) {
  const navigate = useNavigate();
  const link = useDatapackShareLink(datapackId, { enabled });
  const token = link.data?.status === SHARE_LINK_STATUS_ACTIVE ? link.data.token : null;
  return {
    label: token ? "Copy share link" : "Create share link",
    run: () =>
      token
        ? void copyShareLinkUrl(token)
        : navigate(
            `${datapackSharePath(datapackId)}?${SHARE_PAGE_HIGHLIGHT_PARAM}=${SHARE_PAGE_HIGHLIGHT_LINK}`,
          ),
  };
}
