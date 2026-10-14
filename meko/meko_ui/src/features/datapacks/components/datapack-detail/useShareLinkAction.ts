// What the details page's share-link action does: copy an active link, or open
// the share page to create or turn one on. Owner and maintainer only.

import { useNavigate } from "react-router-dom";
import { datapackSharePath } from "@/app/routes/paths";
import { SHARE_LINK_STATUS_ACTIVE } from "@/lib/api/shareLinksApi";
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
    run: () => (token ? void copyShareLinkUrl(token) : navigate(datapackSharePath(datapackId))),
  };
}
