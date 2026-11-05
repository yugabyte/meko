// Shown when Invite is clicked: sharing is a Meko Cloud feature, so this points there.

import { ExternalLink, Users } from "lucide-react";
import { MEKO_CLOUD_URL } from "../lib/links";
import { Banner } from "./Banner";

export function CloudBanner({ onClose }: { onClose: () => void }) {
  return (
    <Banner
      className="mt-4"
      icon={<Users />}
      onClose={onClose}
      action={
        <a
          href={MEKO_CLOUD_URL}
          target="_blank"
          rel="noopener noreferrer"
          className="inline-flex h-7 items-center gap-1.5 rounded-lg bg-primary px-2.5 text-xs font-medium text-primary-foreground hover:opacity-90"
        >
          Open Meko Cloud <ExternalLink className="size-3.5" />
        </a>
      }
    >
      <b>Sharing lives in Meko Cloud.</b> Meko local keeps this datapack on your machine. To invite teammates, use
      Meko Cloud.
    </Banner>
  );
}
