// The Agents page's card: the agents installed on this computer (GET /agents), whether each reaches
// Meko (which sends their calls to the linked Meko Cloud), and Connect or Disconnect, which post the
// agent's route and then read the list again; then where any other MCP client connects, from the
// reply's target. A server older than the route (a 404) gets a note saying what to run instead.

import { useState } from "react";
import { api, ApiError } from "../lib/api";
import type { Agent, AgentState } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { CopyBlock } from "./CopyBlock";
import { Badge, Button, Card, Code, ErrorNote, Loading, Spinner } from "./ui";

type Action = "connect" | "disconnect";

const STATE: Record<AgentState, { label: string; tone: "success" | "warning" | "neutral" }> = {
  connected: { label: "Connected", tone: "success" },
  stale: { label: "Needs reconnecting", tone: "warning" },
  elsewhere: { label: "Connected elsewhere", tone: "neutral" },
  none: { label: "Not connected", tone: "neutral" },
};

const LABEL: Record<Action, string> = { connect: "Connect", disconnect: "Disconnect" };
const DOING: Record<Action, string> = { connect: "Connecting…", disconnect: "Disconnecting…" };

const RESTART = "New sessions have Meko's tools; running ones need a restart";

export function AgentsCard() {
  const list = useLoad(() => api.agents(), []);
  const [busy, setBusy] = useState<{ name: string; action: Action }>();
  // The last action's outcome, shown under its agent: the reply's notes, or the error.
  const [result, setResult] = useState<{ name: string; lines: string[] }>();
  const [error, setError] = useState<{ name: string; error: Error }>();

  if (list.error instanceof ApiError && list.error.status === 404) {
    return (
      <Card className="p-4">
        <p className="text-sm text-muted-foreground">
          This Meko is older than the Agents page: run <Code>meko agent connect &lt;agent&gt;</Code> on this computer, or
          update Meko.
        </p>
      </Card>
    );
  }
  if (!list.data && !list.error) return <Loading />;

  const act = async (agent: Agent, action: Action) => {
    setBusy({ name: agent.name, action });
    setResult(undefined);
    setError(undefined);
    try {
      const reply = action === "connect" ? await api.connectAgent(agent.name) : await api.disconnectAgent(agent.name);
      const lines = [...reply.notes];
      if (action === "connect" && reply.agent.state === "connected") lines.push(RESTART);
      setResult({ name: agent.name, lines });
    } catch (e) {
      setError({ name: agent.name, error: e as Error });
    } finally {
      setBusy(undefined);
      list.reload();
    }
  };

  const data = list.data;
  return (
    <Card className="space-y-3 p-4">
      {data && (
        <p className="text-sm text-muted-foreground">
          Agents connect to Meko on this computer, which reaches Meko Cloud for them while it's linked.
        </p>
      )}
      {list.error && <ErrorNote error={list.error} />}
      {data && data.agents.length === 0 && (
        <p className="text-sm text-muted-foreground">
          No agents found on this computer: Meko works with Claude Code, Codex, Kiro CLI, OpenCode and Cursor
        </p>
      )}
      {data && data.agents.length > 0 && (
        <ul className="divide-y">
          {data.agents.map((a) => (
            <AgentRow
              key={a.name}
              agent={a}
              busy={busy}
              onAct={(action) => void act(a, action)}
              lines={result?.name === a.name ? result.lines : []}
              error={error?.name === a.name ? error.error : undefined}
            />
          ))}
        </ul>
      )}
      {data && (
        <div className="space-y-2 border-t pt-3">
          <p className="text-sm text-muted-foreground">
            Any other MCP client: Streamable HTTP at this URL, with the token <Code>meko token</Code> prints as a Bearer
            header.
          </p>
          <CopyBlock text={data.target.url} />
        </div>
      )}
    </Card>
  );
}

function AgentRow({
  agent,
  busy,
  onAct,
  lines,
  error,
}: {
  agent: Agent;
  busy: { name: string; action: Action } | undefined;
  onAct: (action: Action) => void;
  lines: string[];
  error: Error | undefined;
}) {
  const state = STATE[agent.state] ?? STATE.none;
  const doing = busy?.name === agent.name ? busy.action : undefined;
  const button = (action: Action, primary: boolean) => (
    <Button
      size="sm"
      variant={primary ? "primary" : "outline"}
      disabled={busy !== undefined}
      onClick={() => onAct(action)}
    >
      {doing === action && <Spinner className="size-3.5 text-current" />}
      {doing === action ? DOING[action] : LABEL[action]}
    </Button>
  );
  // A Claude Code folder's override can be in both the row and the reply.
  const notes = [...new Set([...agent.notes, ...lines])];
  return (
    <li className="space-y-2 py-3 first:pt-0 last:pb-0">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-medium">{agent.title}</span>
        <Badge tone={state.tone}>{state.label}</Badge>
        {agent.state === "elsewhere" && agent.url && (
          <span className="min-w-0 break-all font-mono text-xs text-muted-foreground">{agent.url}</span>
        )}
        <div className="ml-auto flex shrink-0 gap-2">
          {agent.state !== "connected" && button("connect", true)}
          {(agent.state === "connected" || agent.state === "stale") && button("disconnect", false)}
        </div>
      </div>
      {notes.map((n, i) => (
        <p key={i} className="break-words text-xs text-muted-foreground">
          {n}
        </p>
      ))}
      {error && <ErrorNote error={error} />}
    </li>
  );
}
