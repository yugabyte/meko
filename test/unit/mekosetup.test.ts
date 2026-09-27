import { createHash } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { tempDir } from '../helpers.js';
import {
  MekoSetupError, ensureMekoInClaude, latestMekoSetup, matchesIntegrity, mekoClaudeGaps, mekoKeyForConfig, runMekoSetup, type MekoSetupPackage,
} from '../../src/server/mekosetup.js';

let dir: string;
beforeEach(() => {
  dir = tempDir('akl-mekosetup-');
});
afterEach(() => rmSync(dir, { recursive: true, force: true }));

describe('Meko in Claude Code: what is missing', () => {
  const claudeDir = () => join(dir, 'claude');
  const settings = (s: unknown) => {
    mkdirSync(claudeDir(), { recursive: true });
    writeFileSync(join(claudeDir(), 'settings.json'), JSON.stringify(s));
  };
  const installPlugin = () => {
    const f = join(claudeDir(), 'plugins', 'installed_plugins.json');
    mkdirSync(join(claudeDir(), 'plugins'), { recursive: true });
    writeFileSync(f, JSON.stringify({ version: 2, plugins: { 'meko-agent-skills@meko-agent-skills': [{ installPath: join(claudeDir(), 'p') }] } }));
    mkdirSync(join(claudeDir(), 'p'), { recursive: true });
  };

  it('lists the plugin, the key and the MCP server when none is there', () => {
    expect(mekoClaudeGaps({ claudeDir: claudeDir(), mcpRegistered: () => false, apiKey: 'k' })).toEqual([
      'the meko-agent-skills plugin',
      "MEKO_API_KEY in Claude Code's settings",
      'the Meko MCP server in Claude Code',
    ]);
  });

  it('is empty once everything is set up with the verified key', () => {
    installPlugin();
    settings({ enabledPlugins: { 'meko-agent-skills@meko-agent-skills': true }, env: { MEKO_API_KEY: 'k' } });
    expect(mekoClaudeGaps({ claudeDir: claudeDir(), mcpRegistered: () => true, apiKey: 'k' })).toEqual([]);
  });

  it('counts a different key in the settings, a disabled plugin, and never a key only in the environment', () => {
    installPlugin();
    settings({ enabledPlugins: { 'meko-agent-skills@meko-agent-skills': true }, env: { MEKO_API_KEY: 'old' } });
    expect(mekoClaudeGaps({ claudeDir: claudeDir(), mcpRegistered: () => true, apiKey: 'new' })).toEqual([
      "the current key in Claude Code's settings (it has a different MEKO_API_KEY)",
    ]);
    settings({ enabledPlugins: { 'meko-agent-skills@meko-agent-skills': false } });
    process.env.MEKO_API_KEY = 'k'; // sessions don't inherit the installer's environment
    try {
      expect(mekoClaudeGaps({ claudeDir: claudeDir(), mcpRegistered: () => true, apiKey: 'k' })).toEqual([
        'the meko-agent-skills plugin (installed, not enabled)',
        "MEKO_API_KEY in Claude Code's settings",
      ]);
    } finally {
      delete process.env.MEKO_API_KEY;
    }
  });
});

describe('the install step for Meko in Claude Code', () => {
  const PKG = { version: '9.9.9', tarball: 't', integrity: 'i' };
  /** Runs the step with stand-ins; `after` are the gaps once the setup has run. */
  function step(o: Partial<{ canAsk: boolean; installDeps: boolean; noInstallDeps: boolean }>, d: { before?: string[]; after?: string[]; answer?: string; code?: number } = {}) {
    const calls: string[] = [];
    let ran = false;
    const promise = ensureMekoInClaude(
      { url: 'https://meko.example/mcp', apiKey: 'k', canAsk: false, installDeps: false, noInstallDeps: false, ...o },
      {
        gaps: () => (ran ? (d.after ?? []) : (d.before ?? ['the meko-agent-skills plugin'])),
        latest: async () => (calls.push('latest'), PKG),
        run: async () => ((ran = true), calls.push('run'), d.code ?? 0),
        ask: async (q) => (calls.push(`ask ${q}`), d.answer ?? 'y'),
        say: (t) => calls.push(`say ${t.split('\n')[0]}`),
        warn: () => undefined,
      },
    );
    return { promise, calls };
  }

  it('does nothing when Meko is already set up', async () => {
    const { promise, calls } = step({}, { before: [] });
    expect(await promise).toBe('ready');
    expect(calls).toEqual([]);
  });

  it('refuses with --no-install-deps, and without --install-deps when it cannot ask', async () => {
    const no = step({ noInstallDeps: true });
    await expect(no.promise).rejects.toThrow(/npx @yugabytedb\/meko-mcp --client claude-code --url https:\/\/meko\.example\/mcp/);
    expect(no.calls).toEqual([]);
    const unattended = step({});
    await expect(unattended.promise).rejects.toThrow(/Rerun with --install-deps/);
    expect(unattended.calls).not.toContain('run');
  });

  it('shows what it will run and stops when the answer is no', async () => {
    const { promise, calls } = step({ canAsk: true }, { answer: 'n' });
    await expect(promise).rejects.toBeInstanceOf(MekoSetupError);
    expect(calls[0]).toBe('latest');
    expect(calls[1]).toMatch(/^say .*@yugabytedb\/meko-mcp 9\.9\.9/);
    expect(calls).toContain('ask Set it up now? (Y/n)');
    expect(calls).not.toContain('run');
  });

  it('runs the setup on yes, or with --install-deps without asking', async () => {
    const yes = step({ canAsk: true });
    expect(await yes.promise).toBe('set-up');
    expect(yes.calls).toContain('run');
    const unattended = step({ installDeps: true });
    expect(await unattended.promise).toBe('set-up');
    expect(unattended.calls.some((c) => c.startsWith('ask'))).toBe(false);
  });

  it('fails when the setup exits non-zero or leaves something missing', async () => {
    await expect(step({ installDeps: true }, { code: 1 }).promise).rejects.toThrow(/didn't finish/);
    await expect(step({ installDeps: true }, { after: ['the Meko MCP server in Claude Code'] }).promise).rejects.toThrow(/still missing: the Meko MCP server/);
  });
});

describe('where the Meko key is kept', () => {
  const base = { url: 'https://mcp.mekodata.ai/mcp', defaultUrl: 'https://mcp.mekodata.ai/mcp', envKey: undefined, settingsUrl: undefined };

  it("keeps no key in config.toml once Claude Code's settings hold it, so an old one can't shadow it", () => {
    expect(mekoKeyForConfig({ ...base, apiKey: 'NEW', typed: true, settingsKey: 'NEW' })).toEqual({ apiKey: '', url: '' });
    // A self-hosted URL stays with it when the settings don't name one.
    expect(mekoKeyForConfig({ ...base, url: 'https://meko.internal/mcp', apiKey: 'K', typed: false, settingsKey: 'K' })).toEqual({ apiKey: '', url: 'https://meko.internal/mcp' });
    expect(mekoKeyForConfig({ ...base, url: 'https://meko.internal/mcp', apiKey: 'K', typed: false, settingsKey: 'K', settingsUrl: 'https://meko.internal/mcp' })).toEqual({ apiKey: '', url: '' });
  });

  it('stores a typed key the settings and environment lack, and otherwise leaves config.toml alone', () => {
    expect(mekoKeyForConfig({ ...base, apiKey: 'K', typed: true, settingsKey: undefined })).toEqual({ apiKey: 'K', url: '' });
    expect(mekoKeyForConfig({ ...base, apiKey: 'K', typed: true, settingsKey: undefined, envKey: 'K' })).toBe('unchanged');
    expect(mekoKeyForConfig({ ...base, apiKey: 'K', typed: false, settingsKey: 'OTHER' })).toBe('unchanged');
  });
});

describe("Meko's setup package", () => {
  /** A stand-in package whose "installer" records its arguments and key, and an npm registry serving it. */
  function fakePackage(script = 'writeFileSync(process.env.OUT, JSON.stringify({ args: process.argv.slice(2), key: process.env.MEKO_API_KEY }));') {
    mkdirSync(join(dir, 'build', 'package', 'bin'), { recursive: true });
    writeFileSync(join(dir, 'build', 'package', 'bin', 'create-meko-setup.mjs'), `import { writeFileSync } from 'node:fs';\n${script}\n`);
    execFileSync('tar', ['-czf', join(dir, 'pkg.tgz'), 'package'], { cwd: join(dir, 'build') });
    const data = readFileSync(join(dir, 'pkg.tgz'));
    const pkg: MekoSetupPackage = { version: '9.9.9', tarball: 'https://registry.example/pkg.tgz', integrity: `sha512-${createHash('sha512').update(data).digest('base64')}` };
    const fetchImpl = async (url: string) => ({
      ok: true,
      status: 200,
      json: async () => (url.endsWith('/latest') ? { version: pkg.version, dist: { tarball: pkg.tarball, integrity: pkg.integrity } } : {}),
      arrayBuffer: async () => data.buffer.slice(data.byteOffset, data.byteOffset + data.byteLength) as ArrayBuffer,
    });
    return { pkg, fetchImpl };
  }

  it('reads the latest version, tarball and integrity from the registry', async () => {
    const { pkg, fetchImpl } = fakePackage();
    expect(await latestMekoSetup(fetchImpl)).toEqual(pkg);
  });

  it('runs the verified package for Claude Code, with the key in the environment rather than the arguments', async () => {
    const { pkg, fetchImpl } = fakePackage();
    const out = join(dir, 'out.json');
    process.env.OUT = out;
    try {
      const code = await runMekoSetup(pkg, { url: 'https://mcp.example/mcp', apiKey: 'secret-key', fetchImpl });
      expect(code).toBe(0);
    } finally {
      delete process.env.OUT;
    }
    const ran = JSON.parse(readFileSync(out, 'utf8'));
    expect(ran.args).toEqual(['--client', 'claude-code', '--url', 'https://mcp.example/mcp', '--yes']);
    expect(ran.key).toBe('secret-key');
    expect(ran.args.join(' ')).not.toContain('secret-key');
  });

  it('refuses a download that does not match the registry integrity, and runs nothing', async () => {
    const { pkg, fetchImpl } = fakePackage();
    const out = join(dir, 'out.json');
    process.env.OUT = out;
    try {
      await expect(runMekoSetup({ ...pkg, integrity: `sha512-${Buffer.alloc(64).toString('base64')}` }, { url: 'u', apiKey: 'k', fetchImpl })).rejects.toThrow(/integrity/);
    } finally {
      delete process.env.OUT;
    }
    expect(existsSync(out)).toBe(false);
  });

  it('passes on the setup exit code', async () => {
    const { pkg, fetchImpl } = fakePackage('process.exit(2);');
    expect(await runMekoSetup(pkg, { url: 'u', apiKey: 'k', fetchImpl })).toBe(2);
  });

  it('hands its output to log line by line when asked (the server has no terminal)', async () => {
    const { pkg, fetchImpl } = fakePackage("process.stdout.write('one\\ntwo'); process.stderr.write('oops\\n'); process.exit(3);");
    const lines: string[] = [];
    expect(await runMekoSetup(pkg, { url: 'u', apiKey: 'k', fetchImpl, log: (l) => lines.push(l) })).toBe(3);
    expect(lines.sort()).toEqual(['one', 'oops', 'two']);
  });

  it('checks integrity strings', () => {
    const data = Buffer.from('hello');
    const good = `sha512-${createHash('sha512').update(data).digest('base64')}`;
    expect(matchesIntegrity(data, good)).toBe(true);
    expect(matchesIntegrity(data, `sha1-abc ${good}`)).toBe(true);
    expect(matchesIntegrity(data, 'sha1-qvTGHdzF6KLavt4PO0gs2a6pQ00=')).toBe(false);
    expect(matchesIntegrity(data, 'garbage')).toBe(false);
  });
});
