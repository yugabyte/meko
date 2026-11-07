// The Settings gear in the sidebar's footer, beside the theme toggle, as Kode's rail has it: what
// Meko does with your Claude Code, usage stats, Meko Cloud and who can reach Meko.

import { Settings } from "lucide-react";
import { href, navigate, SETTINGS } from "../lib/route";
import { cx } from "./ui";

export function SettingsLink({ active }: { active: boolean }) {
  return (
    <a
      href={href(SETTINGS)}
      onClick={(e) => (e.preventDefault(), navigate(SETTINGS))}
      title="Settings"
      aria-label="Settings"
      aria-current={active ? "page" : undefined}
      className={cx(
        "inline-flex h-7 items-center justify-center rounded-lg px-2 transition hover:bg-accent",
        active && "bg-accent text-primary",
      )}
    >
      <Settings className="size-3.5" />
    </a>
  );
}
