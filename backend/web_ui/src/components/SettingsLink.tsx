// The sidebar's link to the Settings page: what Meko does with your Claude Code, usage stats, Meko
// Cloud and who can reach Meko.

import { Settings } from "lucide-react";
import { href, navigate, SETTINGS } from "../lib/route";
import { cx } from "./ui";

export function SettingsLink({ active }: { active: boolean }) {
  return (
    <a
      href={href(SETTINGS)}
      onClick={(e) => (e.preventDefault(), navigate(SETTINGS))}
      className={cx(
        "flex items-center gap-2 rounded-lg px-2 py-1.5 text-sm",
        active ? "bg-accent font-medium" : "text-muted-foreground hover:bg-accent",
      )}
    >
      <Settings className={cx("size-4 shrink-0", active && "text-primary")} />
      Settings
    </a>
  );
}
