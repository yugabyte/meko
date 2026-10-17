import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { kiroHome } from './kiro.js';
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
 * What keeps Kiro's sessions off the Meko AgentK uses (`url`), one sentence each; empty when a
 * Meko server there is AgentK's and sends a token. What Meko's setup for Kiro fixes.
 */
export function kiroMekoGaps(servers: KiroMekoServer[], url: string): string[] {
  const ours = servers.filter((s) => sameMekoUrl(s.url, url));
  if (!servers.length) return ['Kiro has no Meko MCP server.'];
  if (!ours.length) return [`Kiro's Meko MCP server points at ${mekoUrlHost(servers[0].url)}, not ${mekoUrlHost(url)}.`];
  if (!ours.some((s) => s.token)) return ["Kiro's Meko MCP server sends no API key."];
  return [];
}
