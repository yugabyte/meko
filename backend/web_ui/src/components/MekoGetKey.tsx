// Where to get a Meko account and API key: Sign up and Log in links to the Meko web app, the steps
// to make a key, and its Settings page written out to copy for a browser on another device.
// fallback replaces the links when the web app isn't known.

import type { ReactNode } from "react";
import { ExternalLink } from "lucide-react";
import { httpHost, mekoKeyLinks } from "../lib/mekourls";
import { CopyBlock } from "./CopyBlock";

export function MekoGetKey({ webUrl, fallback }: { webUrl: string | null; fallback: ReactNode }) {
  const links = mekoKeyLinks(webUrl);
  return (
    <div className="space-y-2 rounded-lg border bg-muted/40 p-3 text-sm" role="note">
      <p className="font-medium">No Meko Cloud key yet?</p>
      {links ? (
        <>
          <div className="flex flex-wrap items-center gap-2">
            <a
              className="inline-flex h-7 items-center gap-1.5 rounded-lg bg-primary px-2.5 text-xs font-medium text-primary-foreground hover:opacity-90"
              href={links.signup}
              target="_blank"
              rel="noreferrer"
            >
              Sign up <ExternalLink className="size-3.5" />
            </a>
            <a
              className="inline-flex h-7 items-center gap-1.5 rounded-lg border bg-card px-2.5 text-xs font-medium hover:bg-accent"
              href={links.signin}
              target="_blank"
              rel="noreferrer"
            >
              Log in <ExternalLink className="size-3.5" />
            </a>
            <span className="text-xs text-muted-foreground">at {httpHost(links.settings)}</span>
          </div>
          <p className="text-muted-foreground">
            Then open Settings → Security → <i>Generate API key</i>, and paste the key below. On another device? Open:
          </p>
          <CopyBlock text={links.settings} />
        </>
      ) : (
        <p className="text-muted-foreground">{fallback}</p>
      )}
    </div>
  );
}
