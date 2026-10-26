// The sidebar's Meko Cloud rows, after the local ones while Meko Cloud is linked: each opens in Meko
// Cloud's web app in a new tab (never a local route, so a Cloud id never reaches one), ends with the
// shared icon when it was shared with the account, and past CLOUD_ROWS_SHOWN the rest wait behind
// Show all. A listing that failed is one note, linking to Settings › Meko Cloud when the fix is there.

import { useState } from "react";
import { Cloud, Users } from "lucide-react";
import { ApiError } from "../lib/api";
import { CLOUD_ROWS_SHOWN, isShared } from "../lib/cloud";
import { href, navigate } from "../lib/route";
import type { CloudDatapack } from "../lib/types";
import { cx } from "./ui";

const settings = { tab: "overview" as const, view: "meko-cloud" as const };

// The codes whose fix is on Settings › Meko Cloud.
const FIX_IN_SETTINGS = new Set(["CLOUD_KEY_REFUSED", "CLOUD_NO_API_URL"]);

const ROW = "flex items-center gap-2 rounded-lg px-2 py-1.5 text-sm text-muted-foreground";

function CloudRow({ d }: { d: CloudDatapack }) {
  const shared = isShared(d.grant);
  const body = (
    <>
      <Cloud className="size-4 shrink-0" aria-hidden />
      <span className="min-w-0 flex-1 truncate">{d.datapack_name}</span>
      <span className="sr-only">, in Meko Cloud</span>
      {shared && (
        <span title="Shared with you" className="shrink-0">
          <Users className="size-3.5 text-primary" aria-hidden />
          <span className="sr-only">, shared with you</span>
        </span>
      )}
    </>
  );
  if (!d.web_url) {
    return (
      <div className={ROW} title="In Meko Cloud. Set the web app URL in Settings › Meko Cloud to open it from here">
        {body}
      </div>
    );
  }
  return (
    <a
      href={d.web_url}
      target="_blank"
      rel="noopener noreferrer"
      title="In Meko Cloud: opens in a new tab"
      className={cx(ROW, "hover:bg-accent")}
    >
      {body}
    </a>
  );
}

export function CloudDatapackRows({
  datapacks,
  loading,
  error,
}: {
  datapacks: CloudDatapack[];
  loading: boolean;
  error?: Error;
}) {
  const [all, setAll] = useState(false);
  if (error) {
    const fix = error instanceof ApiError && FIX_IN_SETTINGS.has(error.code);
    return (
      <div className="mx-2 my-1 rounded-lg bg-destructive/10 px-2 py-1.5 text-xs text-destructive">
        {error.message}
        {fix && (
          <>
            {" "}
            <a
              href={href(settings)}
              onClick={(e) => (e.preventDefault(), navigate(settings))}
              className="font-medium underline"
            >
              Open Meko Cloud settings
            </a>
          </>
        )}
      </div>
    );
  }
  if (loading && datapacks.length === 0) {
    return <div className={cx(ROW, "italic")}>Loading Meko Cloud…</div>;
  }
  const shown = all ? datapacks : datapacks.slice(0, CLOUD_ROWS_SHOWN);
  return (
    <>
      {shown.map((d) => (
        <CloudRow key={d.datapack_id} d={d} />
      ))}
      {datapacks.length > CLOUD_ROWS_SHOWN && (
        <button
          type="button"
          aria-expanded={all}
          onClick={() => setAll(!all)}
          className="w-full rounded-lg px-2 py-1.5 text-left text-xs font-medium text-primary hover:bg-accent"
        >
          {all ? "Show fewer" : `Show all ${datapacks.length}`}
        </button>
      )}
    </>
  );
}
