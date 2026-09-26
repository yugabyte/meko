import { createHash } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { tempDir } from '../helpers.js';
import { latestMekoSetup, matchesIntegrity, mekoClaudeGaps, runMekoSetup, type MekoSetupPackage } from '../../src/server/mekosetup.js';

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
    expect(mekoClaudeGaps({ claudeDir: claudeDir(), mcpRegistered: () => false, env: {} })).toEqual([
      'the meko-agent-skills plugin',
      "MEKO_API_KEY in Claude Code's settings",
      'the Meko MCP server in Claude Code',
    ]);
  });

  it('is empty once everything is set up, and takes the key from the environment too', () => {
    installPlugin();
    settings({ enabledPlugins: { 'meko-agent-skills@meko-agent-skills': true }, env: { MEKO_API_KEY: 'k' } });
    expect(mekoClaudeGaps({ claudeDir: claudeDir(), mcpRegistered: () => true, env: {} })).toEqual([]);
    settings({ enabledPlugins: { 'meko-agent-skills@meko-agent-skills': false } });
    expect(mekoClaudeGaps({ claudeDir: claudeDir(), mcpRegistered: () => true, env: { MEKO_API_KEY: 'k' } })).toEqual([
      'the meko-agent-skills plugin (installed, not enabled)',
    ]);
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

  it('checks integrity strings', () => {
    const data = Buffer.from('hello');
    const good = `sha512-${createHash('sha512').update(data).digest('base64')}`;
    expect(matchesIntegrity(data, good)).toBe(true);
    expect(matchesIntegrity(data, `sha1-abc ${good}`)).toBe(true);
    expect(matchesIntegrity(data, 'sha1-qvTGHdzF6KLavt4PO0gs2a6pQ00=')).toBe(false);
    expect(matchesIntegrity(data, 'garbage')).toBe(false);
  });
});
