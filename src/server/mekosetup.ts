import { spawn, spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { mekoPluginInstalls } from './capture.js';

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
 * What's missing for Meko to work in Claude Code sessions; empty when it's all there. The key
 * must be in Claude Code's settings.json and be the one just verified: sessions don't inherit
 * the installer's environment (they start under the tmux service), and a stale key there would
 * keep the hooks on it.
 */
export function mekoClaudeGaps(opts: {
  claudeDir: string;
  /** True when Claude Code has an MCP server named `meko` (`claude mcp get meko`). */
  mcpRegistered: () => boolean;
  /** The key AgentK Local just verified with Meko. */
  apiKey: string;
}): string[] {
  const gaps: string[] = [];
  let settings: { enabledPlugins?: Record<string, unknown>; env?: Record<string, unknown> } = {};
  try {
    settings = JSON.parse(readFileSync(join(opts.claudeDir, 'settings.json'), 'utf8')) ?? {};
  } catch {
    /* no settings yet */
  }
  const installed = (mekoPluginInstalls(opts.claudeDir)?.length ?? 0) > 0;
  const enabled = Object.entries(settings.enabledPlugins ?? {}).some(([k, v]) => k.startsWith('meko-agent-skills@') && v === true);
  if (!installed) gaps.push('the meko-agent-skills plugin');
  else if (!enabled) gaps.push('the meko-agent-skills plugin (installed, not enabled)');
  // The hooks in every session read the key from settings.json's env.
  const key = typeof settings.env === 'object' && settings.env ? settings.env.MEKO_API_KEY : undefined;
  if (!(typeof key === 'string' && key)) gaps.push("MEKO_API_KEY in Claude Code's settings");
  else if (key !== opts.apiKey) gaps.push("the current key in Claude Code's settings (it has a different MEKO_API_KEY)");
  if (!opts.mcpRegistered()) gaps.push('the Meko MCP server in Claude Code');
  return gaps;
}

/** `claude mcp get meko` succeeds: Claude Code knows the Meko MCP server. */
export function claudeHasMekoMcp(): boolean {
  const r = spawnSync('claude', ['mcp', 'get', 'meko'], { stdio: 'ignore', timeout: 60_000 });
  return r.status === 0;
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

/**
 * Downloads, verifies and runs Meko's setup for Claude Code. The API key goes in the
 * environment (the package reads MEKO_API_KEY), never on the command line where `ps` shows it.
 * Returns the exit code. Its own output goes straight to the terminal, or line by line to `log`
 * when given (the server has no terminal).
 */
export async function runMekoSetup(
  pkg: MekoSetupPackage,
  opts: { url: string; apiKey: string; fetchImpl?: Fetch; node?: string; log?: (line: string) => void },
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
      const child = spawn(opts.node ?? process.execPath, [join(dir, 'package', 'bin', 'create-meko-setup.mjs'), '--client', 'claude-code', '--url', opts.url, '--yes'], {
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

/** The command to set up Meko for Claude Code by hand, against the server the key belongs to. */
export function manualMekoSetup(url: string): string {
  return `npx ${MEKO_SETUP_PACKAGE} --client claude-code --url ${url}`;
}

export class MekoSetupError extends Error {}

/**
 * The install step: sets up Meko for Claude Code when something is missing, only after saying
 * what will change and asking (or with `installDeps`). Throws MekoSetupError, with the command
 * to run by hand, when it's declined, can't ask, or doesn't finish.
 */
export async function ensureMekoInClaude(
  o: { url: string; apiKey: string; canAsk: boolean; installDeps: boolean; noInstallDeps: boolean },
  d: {
    gaps: () => string[];
    latest: () => Promise<MekoSetupPackage>;
    run: (pkg: MekoSetupPackage) => Promise<number>;
    ask: (question: string, def: string) => Promise<string>;
    say: (text: string) => void;
    warn: (label: string, text: string) => void;
  },
): Promise<'ready' | 'set-up'> {
  const gaps = d.gaps();
  if (!gaps.length) return 'ready';
  const manual = manualMekoSetup(o.url);
  d.warn('Meko in Claude', `missing: ${gaps.join(', ')}`);
  if (o.noInstallDeps) throw new MekoSetupError(`Meko isn't set up for Claude Code. Run: ${manual}`);
  const pkg = await d.latest().catch((e: unknown) => {
    throw new MekoSetupError(`Couldn't look up Meko's setup on npm (${e instanceof Error ? e.message : String(e)}). Run it yourself: ${manual}`);
  });
  d.say(
    `    To set it up, the installer will run Meko's own setup (${MEKO_SETUP_PACKAGE} ${pkg.version}, from npm, integrity-checked):\n` +
      `      ${MEKO_SETUP_PACKAGE} --client claude-code --url ${o.url} --yes\n` +
      "    It registers the Meko MCP server with Claude Code, writes MEKO_MCP_URL and MEKO_API_KEY to\n" +
      "    Claude Code's settings.json (for the Meko hooks), installs the meko-agent-skills plugin, sets\n" +
      "    Meko's status line, and allows Meko's tools in Claude Code's permissions.",
  );
  if (!o.installDeps) {
    if (!o.canAsk) throw new MekoSetupError(`Meko isn't set up for Claude Code. Rerun with --install-deps to set it up, or run: ${manual}`);
    if (/^n/i.test(await d.ask('Set it up now? (Y/n)', 'y'))) throw new MekoSetupError(`Meko isn't set up for Claude Code. Run: ${manual}`);
  }
  const code = await d.run(pkg).catch((e: unknown) => {
    throw new MekoSetupError(`Meko's setup couldn't run (${e instanceof Error ? e.message : String(e)}). Run it yourself: ${manual}`);
  });
  const left = d.gaps();
  if (code !== 0 || left.length) {
    throw new MekoSetupError(`Meko's setup didn't finish${left.length ? ` (still missing: ${left.join(', ')})` : ''}. See its output above, or run it yourself: ${manual}`);
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
