// Shown when Invite is clicked: sharing is a Meko Cloud feature, so this points there.

import { ExternalLink, Users, X } from "lucide-react";
import { MEKO_CLOUD_URL } from "../lib/links";
import { Button } from "./ui";

export function CloudBanner({ onClose }: { onClose: () => void }) {
  return (
    <div className="mt-4 flex flex-wrap items-center gap-3 rounded-lg border border-primary/40 bg-primary/10 px-4 py-3 text-sm">
      <Users className="size-4 shrink-0 text-primary" />
      <p className="min-w-0 flex-1">
        <b>Sharing lives in Meko Cloud.</b> Meko local keeps this datapack on your machine. To invite teammates,
        use Meko Cloud.
      </p>
      <a
        href={MEKO_CLOUD_URL}
        target="_blank"
        rel="noopener noreferrer"
        className="inline-flex h-7 items-center gap-1.5 rounded-lg bg-primary px-2.5 text-xs font-medium text-primary-foreground hover:opacity-90"
      >
        Open Meko Cloud <ExternalLink className="size-3.5" />
      </a>
      <Button size="sm" variant="ghost" onClick={onClose} aria-label="Dismiss">
        <X className="size-3.5" />
      </Button>
    </div>
  );
}
