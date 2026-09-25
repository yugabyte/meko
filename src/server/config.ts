import { existsSync, mkdirSync, readFileSync, writeFileSync, renameSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { homedir } from 'node:os';
import { parse, stringify } from 'smol-toml';
import { expandHome, type Paths } from './paths.js';

export type UpdateChannel = 'stable' | 'prerelease' | 'off';

export interface Config {
  server: { bind: string; port: number };
  tls: { cert: string; key: string };
  repos: { roots: string[]; depth: number; cloneInto: string };
  update: { channel: UpdateChannel; repo: string; requireAttestation: boolean; intervalHours: number };
  meko: { url: string; apiKey: string };
  state: { dbPath: string };
  tmux: { socket: string };
  claude: { command: string };
}

export const DEFAULT_CONFIG: Config = {
  server: { bind: '0.0.0.0', port: 7777 },
  tls: { cert: '', key: '' },
  repos: { roots: ['~/code'], depth: 1, cloneInto: '~/code' },
  update: { channel: 'stable', repo: 'yugabyte/agent-k-local', requireAttestation: false, intervalHours: 6 },
  meko: { url: 'https://mcp.mekodata.ai/mcp', apiKey: '' },
  state: { dbPath: '' },
  tmux: { socket: 'agentk' },
  claude: { command: 'claude' },
};

// TOML uses snake_case keys; the in-memory config uses camelCase.
const snake = (s: string) => s.replace(/[A-Z]/g, (c) => '_' + c.toLowerCase());
const camel = (s: string) => s.replace(/_([a-z])/g, (_, c: string) => c.toUpperCase());

function mapKeys(obj: unknown, fn: (k: string) => string): unknown {
  if (Array.isArray(obj)) return obj.map((v) => mapKeys(v, fn));
  if (obj && typeof obj === 'object') {
    return Object.fromEntries(Object.entries(obj).map(([k, v]) => [fn(k), mapKeys(v, fn)]));
  }
  return obj;
}

function merge<T>(base: T, over: unknown): T {
  if (!over || typeof over !== 'object' || Array.isArray(over)) return base;
  const out: Record<string, unknown> = { ...(base as Record<string, unknown>) };
  for (const [k, v] of Object.entries(over)) {
    if (!(k in out)) continue; // ignore unknown keys
    const b = out[k];
    out[k] = b && typeof b === 'object' && !Array.isArray(b) ? merge(b, v) : v;
  }
  return out as T;
}

export function loadConfig(paths: Paths): Config {
  const cfg = existsSync(paths.config)
    ? merge(structuredClone(DEFAULT_CONFIG), mapKeys(parse(readFileSync(paths.config, 'utf8')), camel))
    : structuredClone(DEFAULT_CONFIG);
  // Environment overrides (used by `npm run dev`); never written back to the file.
  if (process.env.AK_BIND) cfg.server.bind = process.env.AK_BIND;
  if (process.env.AK_PORT) cfg.server.port = Number(process.env.AK_PORT);
  return cfg;
}

export function saveConfig(paths: Paths, cfg: Config): void {
  mkdirSync(dirname(paths.config), { recursive: true });
  const header =
    '# agent-k-local configuration. Settings changed in the web UI are written here too.\n' +
    '# Changes to [server] or [tls] take effect after a restart.\n\n';
  const tmp = paths.config + '.tmp';
  writeFileSync(tmp, header + stringify(mapKeys(cfg, snake) as Record<string, unknown>) + '\n', { mode: 0o600 });
  renameSync(tmp, paths.config);
}

export function dbPath(paths: Paths, cfg: Config): string {
  return cfg.state.dbPath ? expandHome(cfg.state.dbPath) : paths.defaultDb;
}

export function repoRoots(cfg: Config): string[] {
  return cfg.repos.roots.map(expandHome);
}

export function isLoopback(bind: string): boolean {
  return bind === '127.0.0.1' || bind === '::1' || bind === 'localhost';
}

/** Meko credentials: config first, then the environment, then Claude Code's settings.json env. */
export function resolveMeko(cfg: Config): { url: string; apiKey: string } {
  let url = cfg.meko.url || process.env.MEKO_MCP_URL || '';
  let apiKey = cfg.meko.apiKey || process.env.MEKO_API_KEY || '';
  if (!apiKey || !url) {
    const fromClaude = readClaudeSettingsEnv();
    apiKey ||= fromClaude.MEKO_API_KEY ?? '';
    url ||= fromClaude.MEKO_MCP_URL ?? '';
  }
  return { url: url || DEFAULT_CONFIG.meko.url, apiKey };
}

export function readClaudeSettingsEnv(): Record<string, string> {
  try {
    const s = JSON.parse(readFileSync(join(homedir(), '.claude', 'settings.json'), 'utf8'));
    return s && typeof s.env === 'object' ? s.env : {};
  } catch {
    return {};
  }
}
