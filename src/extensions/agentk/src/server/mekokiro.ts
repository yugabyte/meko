import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { kiroDefaultAgentName, kiroHome, readKiroAgent } from './kiro.js';

/**
 * The names of the Meko servers a Kiro session can call, for the `kode` agent's hook matchers
 * (`@<name>`): those named `meko`, or starting with it, in Kiro's MCP config (left out when
 * `disabled`) and in the agent's own `mcpServers` (`agentMcp`); `meko` when there's none. Reads files only.
 */
export function kiroMekoServerNames(agentMcp: unknown, home = kiroHome()): string[] {
  const names = new Set<string>();
  const add = (servers: unknown) => {
    if (!servers || typeof servers !== 'object') return;
    for (const [name, v] of Object.entries(servers as Record<string, unknown>)) {
      if (/^meko[\w-]*$/i.test(name) && !(v && typeof v === 'object' && (v as { disabled?: unknown }).disabled === true)) names.add(name);
    }
  };
  try {
    add((JSON.parse(readFileSync(join(home, 'settings', 'mcp.json'), 'utf8')) as { mcpServers?: unknown } | null)?.mcpServers);
  } catch {
    // No MCP config: the agent's own, if any.
  }
  add(agentMcp);
  return names.size ? [...names].sort() : ['meko'];
}

/**
 * Whether the agent Kiro sessions are built from (the user's default; Kiro's built-in one loads it)
 * loads Kiro's MCP config, where Meko's setup puts its server, or names a Meko server itself.
 */
export function kiroAgentLoadsMcp(home = kiroHome()): boolean {
  const name = kiroDefaultAgentName(home);
  const agent = name ? readKiroAgent(name, home) : null;
  if (!agent || agent.includeMcpJson !== false) return true;
  const own = agent.mcpServers && typeof agent.mcpServers === 'object' ? Object.keys(agent.mcpServers) : [];
  return own.some((k) => /^meko/i.test(k));
}

/**
 * What keeps Kiro's sessions from Meko's tools that's Kode's to check, one sentence each: the
 * agent Kiro sessions are built from doesn't load Kiro's MCP config (`loadsMcp`), where Meko puts
 * its server. What Meko's setup for Kiro fixes, as it makes its own agent the default. Kiro's `meko`
 * MCP server itself is Meko's to report (`mekoagents.ts`). Meko's capture hooks aren't checked: they
 * run only with `[meko] capture_transcripts` on, and Meko's setup puts them in the agent it makes default.
 */
export function kiroMekoGaps(loadsMcp = kiroAgentLoadsMcp()): string[] {
  return loadsMcp ? [] : ["Kiro's default agent doesn't load your MCP servers (includeMcpJson is off), so Kiro sessions don't get Meko's tools."];
}
