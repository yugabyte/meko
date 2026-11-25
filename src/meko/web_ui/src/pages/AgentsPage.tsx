// The Agents page (/agents), opened from the sidebar: the agents on this computer, each connected
// or disconnected in one press, and where any other MCP client connects (AgentsCard). Agents
// connect to Meko itself and pick a datapack on each call, so the page is the install's, not a
// datapack's.

import { AgentsCard } from "../components/AgentsCard";

export function AgentsPage() {
  return (
    <div className="max-w-3xl space-y-4">
      <h2 className="text-lg font-semibold">Agents</h2>
      <AgentsCard />
    </div>
  );
}
