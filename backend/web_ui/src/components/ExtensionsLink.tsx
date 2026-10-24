// The sidebar's Extensions group: the label opens the Extensions page, the chevron shows the
// installed extensions under it. It starts collapsed on every load. A running one with a web UI
// links to it, as a plain link that replaces this page (AgentK refuses to be framed); any other
// links to the Extensions page, where it can be started.

import { useState } from "react";
import { Bot, ChevronRight, Puzzle } from "lucide-react";
import { isInstalled, useExtensions } from "../lib/extensions";
import { href, navigate } from "../lib/route";
import type { Extension } from "../lib/types";
import { cx } from "./ui";

const page = { tab: "overview" as const, view: "extensions" as const };

export function ExtensionIcon({ name, className }: { name: string; className?: string }) {
  const Icon = name === "agentk" ? Bot : Puzzle;
  return <Icon className={cx("size-4 shrink-0", className)} />;
}

export function ExtensionsLink({ active }: { active: boolean }) {
  const [open, setOpen] = useState(false);
  // An older server has no /extensions: the group just has none.
  const installed = (useExtensions().data ?? []).filter(isInstalled);
  return (
    <div>
      <div
        className={cx(
          "flex items-center rounded-lg text-sm",
          active ? "bg-accent font-medium" : "text-muted-foreground hover:bg-accent",
        )}
      >
        <a
          href={href(page)}
          onClick={(e) => (e.preventDefault(), navigate(page))}
          className="flex min-w-0 flex-1 items-center gap-2 px-2 py-1.5"
        >
          <Puzzle className={cx("size-4 shrink-0", active && "text-primary")} />
          Extensions
        </a>
        <button
          type="button"
          onClick={() => setOpen(!open)}
          aria-expanded={open}
          aria-label={open ? "Hide extensions" : "Show extensions"}
          title={open ? "Hide extensions" : "Show extensions"}
          className="mr-1 rounded p-1 text-muted-foreground hover:bg-background/60 hover:text-foreground"
        >
          <ChevronRight className={cx("size-3.5 transition-transform", open && "rotate-90")} />
        </button>
      </div>
      {open && (
        <div className="ml-4 mt-0.5 space-y-0.5 border-l pl-2">
          {installed.length === 0 ? (
            <p className="px-2 py-1.5 text-xs text-muted-foreground">None installed</p>
          ) : (
            installed.map((e) => <Item key={e.name} ext={e} />)
          )}
        </div>
      )}
    </div>
  );
}

function Item({ ext }: { ext: Extension }) {
  const webUI = ext.web_ui ?? true;
  const running = webUI && ext.available && ext.path !== "";
  const className = cx(
    "flex items-center gap-2 rounded-lg px-2 py-1.5 text-sm hover:bg-accent",
    running ? "text-muted-foreground" : "text-muted-foreground/70",
  );
  const content = (
    <>
      <ExtensionIcon name={ext.name} />
      <span className="truncate">{ext.title}</span>
      {webUI && (
        <span
          title={running ? "Running" : "Not running"}
          className={cx("ml-auto size-2 shrink-0 rounded-full", running ? "bg-success" : "bg-muted-foreground/40")}
        />
      )}
    </>
  );
  if (running) {
    return (
      <a href={ext.path} className={className}>
        {content}
      </a>
    );
  }
  return (
    <a href={href(page)} onClick={(e) => (e.preventDefault(), navigate(page))} className={className}>
      {content}
    </a>
  );
}
