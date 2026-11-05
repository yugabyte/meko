import { spawn, spawnSync } from 'node:child_process';
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

/**
 * What Meko's capture needs in Claude Code: the meko-agent-skills plugin, and the server and key
 * its hooks read from Claude Code's settings. Its `meko` MCP server is Meko's to report and connect
 * (GET /api/v1/agents, `mekoagents.ts`).
 */
export interface ClaudeMeko {
  plugin: 'ok' | 'missing' | 'disabled';
  /** settings.json `env`: MEKO_MCP_URL (null when unset, which means Meko Cloud) and MEKO_API_KEY. */
  settings: { url: string | null; token: string | null };
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

function nonBlank(v: unknown): string | null {
  return typeof v === 'string' && v.trim() ? v.trim() : null;
}

/** Reads what Meko's capture needs from Claude Code's settings directory. */
export function readClaudeMeko(claudeDir: string): ClaudeMeko {
  const settings = readJson(join(claudeDir, 'settings.json'));
  const env = isRecord(settings) && isRecord(settings.env) ? settings.env : {};
  const installed = (mekoPluginInstalls(claudeDir)?.length ?? 0) > 0;
  const plugins = isRecord(settings) && isRecord(settings.enabledPlugins) ? settings.enabledPlugins : {};
  const enabled = Object.entries(plugins).some(([k, v]) => k.startsWith('meko-agent-skills@') && v === true);
  return {
    plugin: !installed ? 'missing' : enabled ? 'ok' : 'disabled',
    settings: { url: nonBlank(env.MEKO_MCP_URL), token: nonBlank(env.MEKO_API_KEY) },
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
 * What keeps Meko's capture in Claude Code's sessions off AgentK's Meko, one sentence each; empty
 * when nothing does. It reads files only. The token in its settings is checked apart
 * (`claudeSettingsToken`): it may differ from AgentK's, as long as it works.
 */
export function mekoClaudeGaps(c: ClaudeMeko, url: string): string[] {
  const gaps: string[] = [];
  if (c.plugin === 'missing') gaps.push("The meko-agent-skills plugin isn't installed in Claude Code.");
  else if (c.plugin === 'disabled') gaps.push("The meko-agent-skills plugin is installed in Claude Code but not enabled.");
  // The hooks in every session read the key from settings.json's env: sessions don't inherit
  // the installer's environment (they start under the session host's service).
  if (!c.settings.token) gaps.push("Claude Code's settings have no MEKO_API_KEY.");
  if (!sameMekoUrl(settingsUrl(c), url)) gaps.push(`Claude Code's settings point at ${mekoUrlHost(settingsUrl(c))}, not ${mekoUrlHost(url)}.`);
  return gaps;
}

/** The token Claude Code's hooks use on AgentK's server (not one for another server: that's a gap of its own), or null. */
export function claudeSettingsToken(c: ClaudeMeko, url: string): string | null {
  return c.settings.token && sameMekoUrl(settingsUrl(c), url) ? c.settings.token : null;
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

/** A word for sh: as it is when it's safe bare, else single-quoted. */
function shellWord(s: string): string {
  return /^[\w@%+=:,./~-]+$/.test(s) ? s : `'${s.replace(/'/g, `'\\''`)}'`;
}

/**
 * The command to set up Meko for Claude Code (or `client`) by hand, against the server the key
 * belongs to. `local` is given when that's the Meko on this computer (its live URL and token, not
 * just any server on this computer): the command then reads its token from `meko token` in the
 * user's shell, asking the Meko in `local.home` when known, since nothing else gives it to the
 * package there. For any other server the key stays off it: the package reads MEKO_API_KEY, or
 * signs in through the browser, and the local token goes to no server but its own.
 */
export function manualMekoSetup(url: string, client: MekoSetupClient = 'claude-code', local: { home: string | null } | null = null): string {
  const cmd = `npx ${MEKO_SETUP_PACKAGE} --client ${client} --url ${url}`;
  if (!local) return cmd;
  return `${cmd} --api-key "$(${local.home ? `MEKO_HOME=${shellWord(local.home)} ` : ''}meko token)"`;
}

export class MekoSetupError extends Error {}

/**
 * The install step: sets up Meko for Claude Code (or Kiro, `client`) when something is missing,
 * only after asking (or with `installDeps`); the question names the package and version it will
 * run. Throws MekoSetupError, with the command to run by hand, when it's declined, can't ask, or
 * doesn't finish.
 */
export async function ensureMekoSetup(
  o: {
    url: string;
    apiKey: string;
    canAsk: boolean;
    installDeps: boolean;
    noInstallDeps: boolean;
    client?: MekoSetupClient;
    /** Set when `url` is the Meko on this computer, with its data folder: for the command to run by hand (`manualMekoSetup`). */
    local?: { home: string | null } | null;
  },
  d: {
    /** What's wrong with Meko's capture in the agent, one sentence each (mekoClaudeGaps plus a token that doesn't work, or kiroMekoGaps). */
    gaps: () => Promise<string[]>;
    latest: () => Promise<MekoSetupPackage>;
    run: (pkg: MekoSetupPackage) => Promise<number>;
    /**
     * The setup's own output, when `run` keeps it rather than print it: shown only when the setup
     * fails, so a setup that works leaves the install output tidy.
     */
    output?: () => string[];
    ask: (question: string, def: string) => Promise<string>;
    say: (text: string) => void;
    /**
     * Shows the setup running, under the row's label, until `stop` (`ui.spin`): used when its
     * output is kept (`output`), so nothing else writes to the terminal meanwhile.
     */
    spin?: (label: string, doing: string) => { stop: () => void };
  },
): Promise<'ready' | 'set-up'> {
  const gaps = await d.gaps();
  if (!gaps.length) return 'ready';
  const manual = manualMekoSetup(o.url, o.client, o.local);
  const kiro = o.client === 'kiro';
  const agent = kiro ? 'Kiro' : 'Claude Code';
  // No row says what's missing: the question below does, and the setup fixes it all.
  if (o.noInstallDeps) throw new MekoSetupError(`Meko isn't set up for ${agent}. Run: ${manual}`);
  const pkg = await d.latest().catch((e: unknown) => {
    throw new MekoSetupError(`Couldn't look up Meko's setup on npm (${e instanceof Error ? e.message : String(e)}). Run it yourself: ${manual}`);
  });
  const what = `Meko's setup for ${agent} (${MEKO_SETUP_PACKAGE} ${pkg.version} from npm)`;
  if (!o.installDeps) {
    if (!o.canAsk) throw new MekoSetupError(`Meko isn't set up for ${agent}. Rerun with --install-deps to set it up, or run: ${manual}`);
    if (/^n/i.test(await d.ask(`Run ${what}? (Y/n)`, 'y'))) throw new MekoSetupError(`Meko isn't set up for ${agent}. Run: ${manual}`);
  }
  // A spinner while it runs, which the row that says how it went replaces; its output, when it
  // streams (no `output`), says it's running, so a line only heads it with --install-deps.
  let stop = () => {};
  if (d.output && d.spin) stop = d.spin(kiro ? 'Meko in Kiro' : 'Meko in Claude', `Running ${what}`).stop;
  else if (o.installDeps || d.output) d.say(`    Running ${what}`);
  const showOutput = () => {
    for (const line of d.output?.() ?? []) d.say(`      ${line}`);
  };
  let code: number;
  let left: string[];
  try {
    code = await d.run(pkg).catch((e: unknown) => {
      stop();
      showOutput();
      throw new MekoSetupError(`Meko's setup couldn't run (${e instanceof Error ? e.message : String(e)}). Run it yourself: ${manual}`);
    });
    left = await d.gaps();
  } finally {
    stop();
  }
  if (code !== 0 || left.length) {
    showOutput();
    throw new MekoSetupError(`Meko's setup didn't finish.${left.length ? ` Still: ${left.join(' ')}` : ''} See its output above, or run it yourself: ${manual}`);
  }
  return 'set-up';
}

