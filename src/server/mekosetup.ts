import { spawnSync } from 'node:child_process';
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

/** What's missing for Meko to work in Claude Code sessions; empty when it's all there. */
export function mekoClaudeGaps(opts: {
  claudeDir: string;
  /** True when Claude Code has an MCP server named `meko` (`claude mcp get meko`). */
  mcpRegistered: () => boolean;
  env?: NodeJS.ProcessEnv;
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
  // The hooks in every session read the key from settings.json's env (or the shell's environment).
  const key = typeof settings.env === 'object' && settings.env ? settings.env.MEKO_API_KEY : undefined;
  if (!(typeof key === 'string' && key) && !(opts.env ?? process.env).MEKO_API_KEY) gaps.push("MEKO_API_KEY in Claude Code's settings");
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
 * Returns the exit code; its own output goes straight to the terminal.
 */
export async function runMekoSetup(
  pkg: MekoSetupPackage,
  opts: { url: string; apiKey: string; fetchImpl?: Fetch; node?: string },
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
    const r = spawnSync(opts.node ?? process.execPath, [join(dir, 'package', 'bin', 'create-meko-setup.mjs'), '--client', 'claude-code', '--url', opts.url, '--yes'], {
      stdio: ['ignore', 'inherit', 'inherit'],
      env: { ...process.env, MEKO_API_KEY: opts.apiKey },
      timeout: 10 * 60_000,
    });
    return r.status ?? 1;
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}
