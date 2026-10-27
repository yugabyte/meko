import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { kiroDefaultAgentName, kiroHome, readKiroAgent } from './kiro.js';
import { mekoUrlHost, sameMekoUrl } from './mekosetup.js';

/** A Meko MCP server in Kiro's MCP config: its URL, and whether it sends a token. */
export interface KiroMekoServer {
  name: string;
  url: string;
  token: boolean;
}

/**
 * The Meko servers in Kiro's user-level MCP config (`~/.kiro/settings/mcp.json`), as Meko's setup
 * for Kiro writes them: a remote `{ url, headers }` entry with an `Authorization` header. An entry
 * counts as Meko's by its name (`meko`, or one starting with it). Reads files only.
 */
export function readKiroMeko(home = kiroHome()): KiroMekoServer[] {
  let cfg: unknown;
  try {
    cfg = JSON.parse(readFileSync(join(home, 'settings', 'mcp.json'), 'utf8'));
  } catch {
    return [];
  }
  const servers = cfg && typeof cfg === 'object' ? (cfg as { mcpServers?: unknown }).mcpServers : null;
  if (!servers || typeof servers !== 'object') return [];
  const out: KiroMekoServer[] = [];
  for (const [name, v] of Object.entries(servers as Record<string, unknown>)) {
    if (!/^meko/i.test(name) || !v || typeof v !== 'object') continue;
    const e = v as { url?: unknown; headers?: unknown; disabled?: unknown };
    if (typeof e.url !== 'string' || e.disabled === true) continue;
    const headers = e.headers && typeof e.headers === 'object' ? (e.headers as Record<string, unknown>) : {};
    const auth = Object.entries(headers).find(([k]) => k.toLowerCase() === 'authorization')?.[1];
    out.push({ name, url: e.url, token: typeof auth === 'string' && /\S/.test(auth.replace(/^Bearer\s*/i, '')) });
  }
  return out;
}

/**
 * The names of the Meko servers a Kiro session can call, for the `agentk` agent's hook matchers
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
 * What keeps Kiro's sessions off the Meko AgentK uses (`url`), one sentence each; empty when a
 * Meko server there is AgentK's, sends a token, and the agent Kiro sessions are built from loads it
 * (`loadsMcp`). What Meko's setup for Kiro fixes. Meko's capture hooks aren't checked: they run
 * only with `[meko] capture_transcripts` on, and Meko's setup puts them in the agent it makes default.
 */
export function kiroMekoGaps(servers: KiroMekoServer[], url: string, loadsMcp = true): string[] {
  const ours = servers.filter((s) => sameMekoUrl(s.url, url));
  if (!servers.length) return ['Kiro has no Meko MCP server.'];
  if (!ours.length) return [`Kiro's Meko MCP server points at ${mekoUrlHost(servers[0].url)}, not ${mekoUrlHost(url)}.`];
  if (!ours.some((s) => s.token)) return ["Kiro's Meko MCP server sends no API key."];
  if (!loadsMcp) return ["Kiro's default agent doesn't load your MCP servers (includeMcpJson is off), so Kiro sessions don't get Meko's tools."];
  return [];
}
