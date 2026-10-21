// Shared wiring behind a ?highlight=<key> arrival on the datapack Share page:
// read the param, scroll + ring-pulse the matching section, then drop the
// param so a reload doesn't re-fire it. ShareLinkSection is the only caller
// today; a future section only needs its own key.

import { useCallback } from "react";
import { useSearchParams } from "react-router-dom";
import { useArrivalFlash } from "@/hooks/useArrivalFlash";
import { SHARE_PAGE_HIGHLIGHT_PARAM } from "@/lib/linkSharingFeature";
import { cn } from "@/lib/utils";

type Options = {
  /** The ?highlight=<key> value this section responds to. */
  key: string;
  /** Only arm the flash once the section is settled (data loaded). */
  active: boolean;
  /** Shows up in the useArrivalFlash dev log; not user-visible. */
  debugLabel: string;
};

export function useSharePageHighlight({ key, active, debugLabel }: Options) {
  const [searchParams, setSearchParams] = useSearchParams();
  const highlight = searchParams.get(SHARE_PAGE_HIGHLIGHT_PARAM) === key;
  const clearHighlight = useCallback(
    () =>
      setSearchParams(
        (prev) => {
          const next = new URLSearchParams(prev);
          next.delete(SHARE_PAGE_HIGHLIGHT_PARAM);
          return next;
        },
        { replace: true },
      ),
    [setSearchParams],
  );
  return useArrivalFlash<HTMLDivElement>({
    flashKey: highlight ? key : undefined,
    active: highlight && active,
    onDone: clearHighlight,
    debugLabel,
  });
}

/** Class applied to the outer wrapper of a share-page section so the arrival
 *  pulse treatment is identical across every section. */
export function sharePageFlashClass(flashing: boolean): string {
  return cn(
    "rounded-xl transition-shadow",
    flashing && "ring-4 ring-brand/70 shadow-lg animate-pulse",
  );
}
