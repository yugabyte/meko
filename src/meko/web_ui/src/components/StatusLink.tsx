// The sidebar's link to the Status page, with a dot for the context engine's state: green when
// ready, amber while it starts, red when it failed, grey when the server didn't say.

import { useEffect } from "react";
import { Activity } from "lucide-react";
import { api } from "../lib/api";
import { href, navigate } from "../lib/route";
import { useLoad } from "../lib/useLoad";
import { cx } from "./ui";

const POLL_MS = 30_000;

// "error" is the engine's own state after a failure it stays in (a changed model); "failed" and
// "unavailable" are the server's, for an engine that crashed or never ran.
const DOT: Record<string, string> = {
  ready: "bg-success",
  error: "bg-destructive",
  failed: "bg-destructive",
  unavailable: "bg-destructive",
};

export function StatusLink({ active }: { active: boolean }) {
  const status = useLoad(() => api.server(), []);
  const { reload } = status;
  useEffect(() => {
    const t = setInterval(reload, POLL_MS);
    return () => clearInterval(t);
  }, [reload]);
  // A failed poll greys the dot rather than leaving the last good colour on it.
  const state = status.error ? undefined : status.data?.context_engine?.state;
  const to = { tab: "overview" as const, view: "status" as const };
  return (
    <a
      href={href(to)}
      onClick={(e) => (e.preventDefault(), navigate(to))}
      className={cx(
        "flex items-center gap-2 rounded-lg px-2 py-1.5 text-sm",
        active ? "bg-accent font-medium" : "text-muted-foreground hover:bg-accent",
      )}
    >
      <Activity className={cx("size-4 shrink-0", active && "text-primary")} />
      Status
      <span
        title={state ? `Context engine: ${state}` : "Status unknown"}
        className={cx("ml-auto size-2 rounded-full", state ? (DOT[state] ?? "bg-warning") : "bg-muted-foreground/40")}
      />
    </a>
  );
}
