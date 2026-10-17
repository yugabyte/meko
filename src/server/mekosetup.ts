import { execFile, spawn, spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { mekoPluginInstalls } from './capture.js';
import { DEFAULT_MEKO_URL } from '../shared/mekourls.js';

/**
 * Meko's own setup for Claude Code (the `@yugabytedb/meko-mcp` npm package): it registers the
 * Meko MCP server with Claude Code, writes MEKO_MCP_URL and MEKO_API_KEY to Claude Code's
 * settings.json (where the Meko hooks read them), and installs the meko-agent-skills plugin.
 * Releases bundle Node but not npm, so the package is fetched from the npm registry, checked
 * against the registry's integrity hash, and run with this Node (it has no dependencies).
 */
export const MEKO_SETUP_PACKAGE = '@yugabytedb/meko-mcp';
const REGISTRY = 'https://registry.npmjs.org';

/** Claude Code's user-scope `meko` MCP server (in ~/.claude.json): what its tools call. */
export interface ClaudeMekoServer {
  /** Null for a server without one (a local command): its URL and token can't be checked. */
  url: string | null;
  /** The Bearer token it sends, with `${VAR}` references expanded; null when there's none, or it can't be worked out. */
  token: string | null;
  /**
   * The variable the token comes from when AgentK can't expand it: one that's neither in Claude
   * Code's settings `env` nor in AgentK's own environment (a session may get it from its login
   * shell). Such a token is neither tried nor counted as missing.
   */
  unresolved?: string;
}

/** Where Claude Code keeps its Meko: the plugin, the settings its hooks read, and the MCP server. */
export interface ClaudeMeko {
  plugin: 'ok' | 'missing' | 'disabled';
  /** settings.json `env`: MEKO_MCP_URL (null when unset, which means Meko Cloud) and MEKO_API_KEY. */
  settings: { url: string | null; token: string | null };
  /** The user-scope `meko` MCP server, or null when ~/.claude.json has none. */
  server: ClaudeMekoServer | null;
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function readJson(file: string): unknown {
  try {
    return JSON.parse(readFileSync(file, 'utf8'));
  } catch {
    return null;
  }
}

/**
 * `${VAR}` and `${VAR:-default}` expanded as Claude Code does for MCP headers in a session, where
 * settings.json's `env` is applied over the environment. `unresolved` names a variable that's set
 * in neither, with no default: the session may still have it, from its login shell.
 */
function expandEnv(v: string, env: Record<string, unknown>): { value: string; unresolved?: string } {
  let unresolved: string | undefined;
  const value = v.replace(/\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}/g, (_, name: string, def?: string) => {
    const set = typeof env[name] === 'string' ? (env[name] as string) : process.env[name];
    if (set) return set;
    if (def === undefined) unresolved ??= name;
    return def ?? '';
  });
  return { value, unresolved };
}

function nonBlank(v: unknown): string | null {
  return typeof v === 'string' && v.trim() ? v.trim() : null;
}

/** Reads Claude Code's Meko setup from its settings directory and its global config file. */
export function readClaudeMeko(claudeDir: string, configPath: string): ClaudeMeko {
  const settings = readJson(join(claudeDir, 'settings.json'));
  const env = isRecord(settings) && isRecord(settings.env) ? settings.env : {};
  const installed = (mekoPluginInstalls(claudeDir)?.length ?? 0) > 0;
  const plugins = isRecord(settings) && isRecord(settings.enabledPlugins) ? settings.enabledPlugins : {};
  const enabled = Object.entries(plugins).some(([k, v]) => k.startsWith('meko-agent-skills@') && v === true);
  const cfg = readJson(configPath);
  const entry = isRecord(cfg) && isRecord(cfg.mcpServers) ? cfg.mcpServers.meko : undefined;
  let server: ClaudeMekoServer | null = null;
  if (isRecord(entry)) {
    const headers = isRecord(entry.headers) ? entry.headers : {};
    const auth = Object.entries(headers).find(([k]) => k.toLowerCase() === 'authorization')?.[1];
    const expanded = typeof auth === 'string' ? expandEnv(auth, env) : null;
    const bearer = expanded && !expanded.unresolved ? /^\s*Bearer\s+(\S+)\s*$/i.exec(expanded.value) : null;
    server = { url: nonBlank(entry.url), token: bearer?.[1] ?? null, ...(expanded?.unresolved ? { unresolved: expanded.unresolved } : {}) };
  }
  return {
    plugin: !installed ? 'missing' : enabled ? 'ok' : 'disabled',
    settings: { url: nonBlank(env.MEKO_MCP_URL), token: nonBlank(env.MEKO_API_KEY) },
    server,
  };
}

/** The same Meko MCP server: compared without a trailing slash, and with the host's case ignored. */
export function sameMekoUrl(a: string, b: string): boolean {
  const norm = (u: string) => {
    try {
      const x = new URL(u.trim());
      return `${x.protocol}//${x.host}${x.pathname.replace(/\/+$/, '')}${x.search}`;
    } catch {
      return u.trim().replace(/\/+$/, '');
    }
  };
  return norm(a) === norm(b);
}

/** A URL's host, for messages: never the whole URL, which can carry credentials. */
export function mekoUrlHost(url: string): string {
  try {
    return new URL(url).host || 'another server';
  } catch {
    return 'another server';
  }
}

/** The URL Claude Code's hooks use: MEKO_MCP_URL from its settings, else Meko Cloud. */
function settingsUrl(c: ClaudeMeko): string {
  return c.settings.url ?? DEFAULT_MEKO_URL;
}

/**
 * What keeps Claude Code's sessions off AgentK's Meko, one sentence each; empty when nothing does.
 * It reads files only. The tokens are checked apart (claudeMekoTokens): they may differ from
 * AgentK's, as long as they work. `mcpRegistered` is asked only when ~/.claude.json has no
 * user-scope `meko` server (it may be registered some other way); null means it couldn't be asked,
 * which isn't a gap.
 */
export function mekoClaudeGaps(c: ClaudeMeko, url: string, mcpRegistered: () => boolean | null): string[] {
  const gaps: string[] = [];
  const ours = mekoUrlHost(url);
  if (c.plugin === 'missing') gaps.push("The meko-agent-skills plugin isn't installed in Claude Code.");
  else if (c.plugin === 'disabled') gaps.push("The meko-agent-skills plugin is installed in Claude Code but not enabled.");
  // The hooks in every session read the key from settings.json's env: sessions don't inherit
  // the installer's environment (they start under the session host's service).
  if (!c.settings.token) gaps.push("Claude Code's settings have no MEKO_API_KEY.");
  if (!sameMekoUrl(settingsUrl(c), url)) gaps.push(`Claude Code's settings point at ${mekoUrlHost(settingsUrl(c))}, not ${ours}.`);
  if (c.server) {
    if (c.server.url && !sameMekoUrl(c.server.url, url)) gaps.push(`Claude Code's meko MCP server points at ${mekoUrlHost(c.server.url)}, not ${ours}.`);
    else if (c.server.url && !c.server.token && !c.server.unresolved) gaps.push("Claude Code's meko MCP server sends no token.");
  } else if (mcpRegistered() === false) {
    gaps.push('Claude Code has no meko MCP server.');
  }
  return gaps;
}

/** Where Claude Code holds a Meko token. */
export type ClaudeTokenPlace = 'settings' | 'server';

/** The tokens Claude Code uses on AgentK's server (not those on another server: that's a gap of its own). */
export function claudeMekoTokens(c: ClaudeMeko, url: string): { place: ClaudeTokenPlace; token: string }[] {
  const out: { place: ClaudeTokenPlace; token: string }[] = [];
  if (c.settings.token && sameMekoUrl(settingsUrl(c), url)) out.push({ place: 'settings', token: c.settings.token });
  if (c.server?.url && c.server.token && sameMekoUrl(c.server.url, url)) out.push({ place: 'server', token: c.server.token });
  return out;
}

/**
 * `<claude> mcp get meko` succeeds: Claude Code knows the Meko MCP server. Blocks: only for the
 * installer, where anything but success counts as not set up (it asks before setting it up).
 */
export function claudeHasMekoMcp(command: string): boolean {
  const r = spawnSync(command, ['mcp', 'get', 'meko'], { stdio: 'ignore', timeout: 60_000 });
  return r.status === 0;
}

/**
 * claudeHasMekoMcp without blocking, for the server: true when it's there, false when Claude Code
 * says it isn't (exit 1), and null when Claude Code couldn't be asked (not found, timed out, another
 * failure), which mustn't read as a missing server.
 */
export function claudeHasMekoMcpAsync(command: string): Promise<boolean | null> {
  return new Promise((resolve) => {
    execFile(command, ['mcp', 'get', 'meko'], { timeout: 60_000 }, (err) => {
      if (!err) return resolve(true);
      // An exit status is a number here; a command that couldn't start has a string code (ENOENT).
      const { code, killed } = err as { code?: unknown; killed?: boolean };
      resolve(code === 1 && !killed ? false : null);
    });
  });
}

/**
 * The installer's own check on the key it was given in its environment (MEKO_API_KEY, not typed):
 * it isn't stored in config.toml, so once AgentK runs as a service (with no MEKO_* environment) it
 * takes the key from Claude Code's settings. A different key there would become AgentK's.
 */
export function envKeyGap(c: ClaudeMeko, apiKey: string, keyFromEnv: boolean): string[] {
  return keyFromEnv && c.settings.token && c.settings.token !== apiKey
    ? ["Claude Code's settings hold a different MEKO_API_KEY, which AgentK would use instead of this one once it runs as a service."]
    : [];
}

export interface MekoSetupPackage {
  version: string;
  tarball: string;
  integrity: string;
}

type Fetch = (url: string, init?: { signal?: AbortSignal }) => Promise<{ ok: boolean; status: number; json(): Promise<unknown>; arrayBuffer(): Promise<ArrayBuffer> }>;

/** The latest published setup package: its version, tarball URL and integrity hash. */
export async function latestMekoSetup(fetchImpl: Fetch = fetch): Promise<MekoSetupPackage> {
  const res = await fetchImpl(`${REGISTRY}/${MEKO_SETUP_PACKAGE.replace('/', '%2f')}/latest`, { signal: AbortSignal.timeout(30_000) });
  if (!res.ok) throw new Error(`the npm registry answered ${res.status} for ${MEKO_SETUP_PACKAGE}`);
  const m = (await res.json()) as { version?: unknown; dist?: { tarball?: unknown; integrity?: unknown } };
  if (typeof m.version !== 'string' || typeof m.dist?.tarball !== 'string' || typeof m.dist.integrity !== 'string') {
    throw new Error(`the npm registry's entry for ${MEKO_SETUP_PACKAGE} is missing its version, tarball or integrity`);
  }
  return { version: m.version, tarball: m.dist.tarball, integrity: m.dist.integrity };
}

/** True when `data` matches an npm integrity string (`sha512-<base64>`, possibly several). */
export function matchesIntegrity(data: Buffer, integrity: string): boolean {
  return integrity.split(/\s+/).some((entry) => {
    const [alg, want] = [entry.slice(0, entry.indexOf('-')), entry.slice(entry.indexOf('-') + 1)];
    if (!['sha512', 'sha384', 'sha256'].includes(alg) || !want) return false;
    return createHash(alg).update(data).digest('base64') === want;
  });
}

/** The coding agents Meko's setup package sets up (its `--client`). */
export type MekoSetupClient = 'claude-code' | 'kiro';

/**
 * Downloads, verifies and runs Meko's setup for Claude Code (or `client`). The API key goes in the
 * environment (the package reads MEKO_API_KEY), never on the command line where `ps` shows it.
 * Returns the exit code. Its own output goes straight to the terminal, or line by line to `log`
 * when given (the server has no terminal).
 */
export async function runMekoSetup(
  pkg: MekoSetupPackage,
  opts: { url: string; apiKey: string; client?: MekoSetupClient; fetchImpl?: Fetch; node?: string; log?: (line: string) => void },
): Promise<number> {
  const res = await (opts.fetchImpl ?? fetch)(pkg.tarball, { signal: AbortSignal.timeout(120_000) });
  if (!res.ok) throw new Error(`downloading ${MEKO_SETUP_PACKAGE} ${pkg.version} failed (${res.status})`);
  const data = Buffer.from(await res.arrayBuffer());
  if (!matchesIntegrity(data, pkg.integrity)) throw new Error(`${MEKO_SETUP_PACKAGE} ${pkg.version} doesn't match the npm registry's integrity hash`);
  const dir = mkdtempSync(join(tmpdir(), 'akl-meko-setup-'));
  try {
    writeFileSync(join(dir, 'package.tgz'), data);
    const untar = spawnSync('tar', ['-xzf', 'package.tgz'], { cwd: dir, stdio: 'ignore' });
    if (untar.status !== 0) throw new Error(`couldn't unpack ${MEKO_SETUP_PACKAGE} ${pkg.version}`);
    return await new Promise<number>((resolve, reject) => {
      const log = opts.log;
      const child = spawn(opts.node ?? process.execPath, [join(dir, 'package', 'bin', 'create-meko-setup.mjs'), '--client', opts.client ?? 'claude-code', '--url', opts.url, '--yes'], {
        stdio: ['ignore', log ? 'pipe' : 'inherit', log ? 'pipe' : 'inherit'],
        env: { ...process.env, MEKO_API_KEY: opts.apiKey },
        timeout: 10 * 60_000,
      });
      if (log) {
        for (const out of [child.stdout, child.stderr]) {
          let rest = '';
          out?.setEncoding('utf8');
          out?.on('data', (chunk: string) => {
            const lines = (rest + chunk).split('\n');
            rest = lines.pop() ?? '';
            for (const l of lines) if (l.trim()) log(l);
          });
          out?.on('end', () => rest.trim() && log(rest));
        }
      }
      child.on('error', reject);
      child.on('close', (code) => resolve(code ?? 1));
    });
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

/** The command to set up Meko for Claude Code (or `client`) by hand, against the server the key belongs to. */
export function manualMekoSetup(url: string, client: MekoSetupClient = 'claude-code'): string {
  return `npx ${MEKO_SETUP_PACKAGE} --client ${client} --url ${url}`;
}

export class MekoSetupError extends Error {}

/**
 * The install step: sets up Meko for Claude Code when something is missing, only after asking
 * (or with `installDeps`); the question names the package and version it will run. Throws
 * MekoSetupError, with the command to run by hand, when it's declined, can't ask, or doesn't finish.
 */
export async function ensureMekoInClaude(
  o: { url: string; apiKey: string; canAsk: boolean; installDeps: boolean; noInstallDeps: boolean },
  d: {
    /** What's wrong with Meko in Claude Code, one sentence each (mekoClaudeGaps, plus tokens that don't work). */
    gaps: () => Promise<string[]>;
    latest: () => Promise<MekoSetupPackage>;
    run: (pkg: MekoSetupPackage) => Promise<number>;
    ask: (question: string, def: string) => Promise<string>;
    say: (text: string) => void;
    warn: (label: string, text: string) => void;
  },
): Promise<'ready' | 'set-up'> {
  const gaps = await d.gaps();
  if (!gaps.length) return 'ready';
  const manual = manualMekoSetup(o.url);
  // A short word, not the list: the install output stays tidy, and the setup fixes them all.
  d.warn('Meko in Claude', 'not set up for this Meko');
  if (o.noInstallDeps) throw new MekoSetupError(`Meko isn't set up for Claude Code. Run: ${manual}`);
  const pkg = await d.latest().catch((e: unknown) => {
    throw new MekoSetupError(`Couldn't look up Meko's setup on npm (${e instanceof Error ? e.message : String(e)}). Run it yourself: ${manual}`);
  });
  const what = `Meko's setup (${MEKO_SETUP_PACKAGE} ${pkg.version} from npm)`;
  if (o.installDeps) {
    d.say(`    Running ${what}`);
  } else {
    if (!o.canAsk) throw new MekoSetupError(`Meko isn't set up for Claude Code. Rerun with --install-deps to set it up, or run: ${manual}`);
    if (/^n/i.test(await d.ask(`Run ${what}? (Y/n)`, 'y'))) throw new MekoSetupError(`Meko isn't set up for Claude Code. Run: ${manual}`);
  }
  const code = await d.run(pkg).catch((e: unknown) => {
    throw new MekoSetupError(`Meko's setup couldn't run (${e instanceof Error ? e.message : String(e)}). Run it yourself: ${manual}`);
  });
  const left = await d.gaps();
  if (code !== 0 || left.length) {
    throw new MekoSetupError(`Meko's setup didn't finish.${left.length ? ` Still: ${left.join(' ')}` : ''} See its output above, or run it yourself: ${manual}`);
  }
  return 'set-up';
}

/**
 * Where the verified key is kept in config.toml. When Claude Code's settings hold it, config.toml
 * keeps none: config comes first when the server resolves the key, so an older key left there
 * would shadow the one the hooks use. Otherwise a key the user typed is stored there.
 */
export function mekoKeyForConfig(o: {
  apiKey: string; url: string; typed: boolean; settingsKey: unknown; settingsUrl: unknown; envKey: string | undefined; defaultUrl: string;
}): { apiKey: string; url: string } | 'unchanged' {
  const url = o.url === o.defaultUrl ? '' : o.url;
  // The URL goes with the key: keep a non-default one here only when the settings don't name one.
  if (o.settingsKey === o.apiKey) return { apiKey: '', url: o.settingsUrl ? '' : url };
  if (o.typed && o.envKey !== o.apiKey) return { apiKey: o.apiKey, url };
  return 'unchanged';
}
