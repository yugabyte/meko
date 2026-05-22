import { useEffect, useState } from "react";
import { Bot } from "lucide-react";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";
import { getApiErrorMessage, listAgents, type Agent } from "@/lib/mekoApi";

const PREVIEW_LIMIT = 3;

function resolvedAgentId(a: Agent & { id?: string }): string {
  return a.agent_id?.trim() || a.id?.trim() || "—";
}

function parseAgentDetailsName(details: unknown): string | null {
  if (details == null) return null;
  if (typeof details === "string") {
    try {
      const o = JSON.parse(details) as { name?: string; display_name?: string };
      return o.display_name?.trim() || o.name?.trim() || null;
    } catch {
      return null;
    }
  }
  if (typeof details === "object") {
    const o = details as { name?: string; display_name?: string };
    return o.display_name?.trim() || o.name?.trim() || null;
  }
  return null;
}

function agentRowLabel(a: Agent & { agent_name?: string }): string {
  const fromDetails = parseAgentDetailsName(a.agent_details);
  if (fromDetails) return fromDetails;
  const named = a.agent_name?.trim();
  if (named) return named;
  return resolvedAgentId(a);
}

type ListStatus = "loading" | "ready" | "error";

type Props = {
  datapackName: string;
  datapackId?: string;
  className?: string;
};

export function DatapackDetailAgentsTile({ datapackName, datapackId, className }: Props) {
  const [agents, setAgents] = useState<Agent[]>([]);
  const [status, setStatus] = useState<ListStatus>("loading");
  const [errorMessage, setErrorMessage] = useState<string | null>(null);

  useEffect(() => {
    const ac = new AbortController();
    setStatus("loading");
    setErrorMessage(null);
    void listAgents(datapackName, { signal: ac.signal, datapackId })
      .then((rows) => {
        if (ac.signal.aborted) return;
        setAgents(rows ?? []);
        setStatus("ready");
      })
      .catch((e) => {
        if (ac.signal.aborted) return;
        setAgents([]);
        setErrorMessage(getApiErrorMessage(e, "Could not load agents."));
        setStatus("error");
      });
    return () => ac.abort();
  }, [datapackName, datapackId]);

  const total = agents.length;
  const preview = agents.slice(0, PREVIEW_LIMIT);
  const countLabel = status === "loading" ? "…" : String(total);

  return (
    <aside
      className={cn(
        "flex h-full min-h-0 w-full flex-col rounded-xl border border-border bg-card/50 shadow-sm",
        className,
      )}
    >
      <div className="flex items-center gap-2 border-b border-border px-4 py-3">
        <Bot className="h-4 w-4 shrink-0 text-violet-400" aria-hidden />
        <span className="truncate text-sm font-semibold text-foreground">Agents</span>
        <span className="rounded-full bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground tabular-nums">
          {countLabel}
        </span>
      </div>
      <ul className="flex flex-col divide-y divide-border px-2 py-1">
        {status === "loading" ? (
          <li className="px-2 py-4 text-center text-xs text-muted-foreground">Loading agents…</li>
        ) : status === "error" ? (
          <li className="px-2 py-4 text-center text-xs leading-relaxed text-muted-foreground">{errorMessage}</li>
        ) : preview.length === 0 ? (
          <li className="px-2 py-4 text-center text-xs text-muted-foreground">No agents linked yet.</li>
        ) : (
          preview.map((a) => {
            const row = a as Agent & { id?: string; agent_name?: string };
            const label = agentRowLabel(row);
            const id = resolvedAgentId(row);
            return (
              <li key={id} className="flex items-center gap-3 px-2 py-2.5">
                <div
                  // Soft violet pill in light mode (`bg-violet-100`),
                  // saturated dark-violet in dark mode. The original
                  // `violet-950/70` on white rendered as a near-solid
                  // dark circle — gaudy against the white card.
                  className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-violet-100 dark:bg-violet-950/70 text-violet-700 dark:text-violet-200 ring-1 ring-violet-600/40"
                  aria-hidden
                >
                  <Bot className="h-4 w-4 opacity-90" />
                </div>
                <div className="min-w-0 flex-1">
                  <p className="truncate text-sm font-medium text-foreground" title={label}>
                    {label}
                  </p>
                  {label !== id ? (
                    <p className="truncate font-mono text-[11px] text-muted-foreground" title={id}>
                      {id}
                    </p>
                  ) : null}
                </div>
              </li>
            );
          })
        )}
      </ul>
      <div className="border-t border-border">
        <Tooltip>
          <TooltipTrigger asChild>
            <span className="inline-flex w-full">
              <button
                type="button"
                className="w-full py-3 text-center text-sm font-normal text-muted-foreground/75 transition-colors disabled:cursor-not-allowed disabled:hover:text-muted-foreground/75"
                disabled
                aria-label="Invite agent (coming soon)"
              >
                + Invite agent
              </button>
            </span>
          </TooltipTrigger>
          <TooltipContent side="top" className="max-w-[14rem]">
            Coming soon
          </TooltipContent>
        </Tooltip>
      </div>
    </aside>
  );
}
