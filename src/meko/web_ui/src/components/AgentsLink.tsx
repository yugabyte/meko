// The sidebar's link to the Agents page, under Status: the agents on this computer, which connect to
// Meko itself, not to a datapack.

import { Bot } from "lucide-react";
import { AGENTS, href, navigate } from "../lib/route";
import { cx } from "./ui";

export function AgentsLink({ active }: { active: boolean }) {
  return (
    <a
      href={href(AGENTS)}
      onClick={(e) => (e.preventDefault(), navigate(AGENTS))}
      aria-current={active ? "page" : undefined}
      className={cx(
        "flex items-center gap-2 rounded-lg px-2 py-1.5 text-sm",
        active ? "bg-accent font-medium" : "text-muted-foreground hover:bg-accent",
      )}
    >
      <Bot className={cx("size-4 shrink-0", active && "text-primary")} />
      Agents
    </a>
  );
}
