// What the datapack header's share-link button does: copy an active link,
// open the one-shot create modal when the datapack has no link at all, or
// route to the Share page when a link exists but is turned off. Owner only
// on the surfaces that use this today (link sharing is owner-gated).

import { useNavigate } from "react-router-dom";
import { datapackSharePath } from "@/app/routes/paths";
import { SHARE_LINK_STATUS_ACTIVE } from "@/lib/api/shareLinksApi";
import { SHARE_PAGE_HIGHLIGHT_LINK, SHARE_PAGE_HIGHLIGHT_PARAM } from "@/lib/linkSharingFeature";
import {
  copyShareLinkUrl,
  useDatapackShareLink,
} from "@/features/datapacks/hooks/useDatapackShareLink";

type Options = {
  /** Opens the one-shot create modal (CreateShareLinkDialog). Only called
   *  when the datapack has no share link at all. For an off-but-exists link
   *  we deliberately bypass this and route to the Share page instead — the
   *  modal would send a default grant on top of the saved one, overwriting
   *  the role and moving every member who joined through that link. The
   *  Share page's Switch sends {status} alone (saved grant preserved) and
   *  also shows Reset for a leaked token. */
  onCreate?: () => void;
};

export function useShareLinkAction(datapackId: string, enabled: boolean, options?: Options) {
  const navigate = useNavigate();
  const link = useDatapackShareLink(datapackId, { enabled });
  const activeToken = link.data?.status === SHARE_LINK_STATUS_ACTIVE ? link.data.token : null;
  // link.data === null is the "truly no link yet" case; link.data with any
  // status is "a row exists" (active OR off). The modal is only safe for the
  // first; the second lives on the Share page so the saved grant is intact.
  const hasRow = link.data != null;
  return {
    label: activeToken ? "Copy share link" : "Create share link",
    run: () => {
      if (activeToken) {
        void copyShareLinkUrl(activeToken);
        return;
      }
      if (options?.onCreate && link.isSuccess && !hasRow) {
        options.onCreate();
        return;
      }
      // Either the query hasn't resolved, or a row already exists and is off.
      // Both paths want the Share page's link card: the first so the user
      // isn't guessing at state, the second so the Switch + Reset affordances
      // handle a controlled turn-on without clobbering the stored grant.
      navigate(
        `${datapackSharePath(datapackId)}?${SHARE_PAGE_HIGHLIGHT_PARAM}=${SHARE_PAGE_HIGHLIGHT_LINK}`,
      );
    },
  };
}
