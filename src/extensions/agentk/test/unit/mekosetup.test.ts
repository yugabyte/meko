import { createHash } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { tempDir } from '../helpers.js';
import {
  MekoSetupError, claudeSettingsToken, ensureMekoSetup, latestMekoSetup, manualMekoSetup, matchesIntegrity, mekoClaudeGaps, readClaudeMeko,
  runMekoSetup, sameMekoUrl, type MekoSetupPackage,
} from '../../src/server/mekosetup.js';
import { defaultMekoTools } from '../../src/server/mekosettings.js';

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

  const CLOUD = 'https://mcp.mekodata.ai/mcp';
  const OWN = 'https://meko.example.com/mcp';
  const read = () => readClaudeMeko(claudeDir());
  const ready = (env: Record<string, string> = { MEKO_API_KEY: 'k' }) => {
    installPlugin();
    settings({ enabledPlugins: { 'meko-agent-skills@meko-agent-skills': true }, env });
  };

  it('lists the plugin and the key when neither is there (the MCP server is Meko\'s to report)', () => {
    expect(mekoClaudeGaps(read(), CLOUD)).toEqual([
      "The meko-agent-skills plugin isn't installed in Claude Code.",
      "Claude Code's settings have no MEKO_API_KEY.",
    ]);
  });

  it('is empty when the settings use this server, whatever token they hold, which is the one tried', () => {
    ready({ MEKO_API_KEY: 'settings-token' });
    expect(mekoClaudeGaps(read(), CLOUD)).toEqual([]);
    expect(claudeSettingsToken(read(), 'https://MCP.MekoData.ai/mcp')).toBe('settings-token');
  });

  it('counts a disabled plugin, and never a key only in the environment', () => {
    installPlugin();
    settings({ enabledPlugins: { 'meko-agent-skills@meko-agent-skills': false } });
    process.env.MEKO_API_KEY = 'k'; // sessions don't inherit the installer's environment
    try {
      expect(mekoClaudeGaps(read(), CLOUD)).toEqual([
        "The meko-agent-skills plugin is installed in Claude Code but not enabled.",
        "Claude Code's settings have no MEKO_API_KEY.",
      ]);
    } finally {
      delete process.env.MEKO_API_KEY;
    }
  });

  it('names the other server when the settings point elsewhere, by host only, and tries no token there', () => {
    ready({ MEKO_API_KEY: 'k', MEKO_MCP_URL: 'https://user:secret@meko.example.com/mcp?x=1' });
    const gaps = mekoClaudeGaps(read(), CLOUD);
    expect(gaps).toEqual(["Claude Code's settings point at meko.example.com, not mcp.mekodata.ai."]);
    expect(gaps.join(' ')).not.toMatch(/secret|user/);
    expect(claudeSettingsToken(read(), CLOUD)).toBe(null);
  });

  it('counts unset MEKO_MCP_URL as Meko Cloud, which the hooks then use', () => {
    ready();
    expect(mekoClaudeGaps(read(), OWN)).toEqual(["Claude Code's settings point at mcp.mekodata.ai, not meko.example.com."]);
  });

  it('compares URLs without a trailing slash and with the host in any case', () => {
    expect(sameMekoUrl('https://MCP.mekodata.ai/mcp/', CLOUD)).toBe(true);
    expect(sameMekoUrl('https://mcp.mekodata.ai/other', CLOUD)).toBe(false);
    expect(sameMekoUrl('http://mcp.mekodata.ai/mcp', CLOUD)).toBe(false);
  });
});

describe('the command to set up Meko by hand', () => {
  it("reads the local Meko's token in the user's shell, from the Meko AgentK uses, and keeps any other server's key off it", () => {
    expect(manualMekoSetup('http://127.0.0.1:7778/mcp', 'claude-code', { home: null })).toBe(
      'npx @yugabytedb/meko-mcp --client claude-code --url http://127.0.0.1:7778/mcp --api-key "$(meko token)"',
    );
    expect(manualMekoSetup('http://127.0.0.1:7778/mcp', 'kiro', { home: "/data/it's meko" })).toBe(
      `npx @yugabytedb/meko-mcp --client kiro --url http://127.0.0.1:7778/mcp --api-key "$(MEKO_HOME='/data/it'\\''s meko' meko token)"`,
    );
    // Another server on this computer isn't the local Meko: its token goes nowhere else.
    expect(manualMekoSetup('http://127.0.0.1:8080/mcp', 'claude-code', null)).toBe('npx @yugabytedb/meko-mcp --client claude-code --url http://127.0.0.1:8080/mcp');
  });
});

describe('the install step for Meko in Claude Code', () => {
  const PKG = { version: '9.9.9', tarball: 't', integrity: 'i' };
  /** Runs the step with stand-ins; `after` are the gaps once the setup has run. */
  function step(o: Partial<{ canAsk: boolean; installDeps: boolean; noInstallDeps: boolean }>, d: { before?: string[]; after?: string[]; answer?: string; code?: number } = {}) {
    const calls: string[] = [];
    let ran = false;
    const promise = ensureMekoSetup(
      { url: 'https://meko.example/mcp', apiKey: 'k', canAsk: false, installDeps: false, noInstallDeps: false, ...o },
      {
        gaps: async () => (ran ? (d.after ?? []) : (d.before ?? ['The meko-agent-skills plugin isn\'t installed in Claude Code.'])),
        latest: async () => (calls.push('latest'), PKG),
        run: async () => ((ran = true), calls.push('run'), d.code ?? 0),
        ask: async (q) => (calls.push(`ask ${q}`), d.answer ?? 'y'),
        say: (t) => calls.push(`say ${t.split('\n')[0]}`),
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

  it('asks with the package it will run, and stops when the answer is no', async () => {
    const { promise, calls } = step({ canAsk: true }, { answer: 'n' });
    await expect(promise).rejects.toBeInstanceOf(MekoSetupError);
    expect(calls).toEqual(['latest', "ask Run Meko's setup for Claude Code (@yugabytedb/meko-mcp 9.9.9 from npm)? (Y/n)"]);
  });

  it('shows a spinner under the row while the setup runs, and stops it before anything else is said', async () => {
    for (const code of [0, 1]) {
      const seen: string[] = [];
      let ran = false;
      const done = ensureMekoSetup(
        { url: 'u', apiKey: 'k', canAsk: true, installDeps: false, noInstallDeps: false, client: 'kiro' },
        {
          gaps: async () => (ran && code === 0 ? [] : ['x']), latest: async () => PKG,
          run: async () => ((ran = true), seen.push('run'), code), output: () => ['Restart Kiro'],
          ask: async () => 'y', say: (t) => seen.push(`say ${t}`),
          spin: (l, t) => (seen.push(`spin ${l}: ${t}`), { stop: () => seen.push('stop') }),
        },
      );
      if (code === 0) expect(await done).toBe('set-up');
      else await expect(done).rejects.toThrow(/didn't finish/);
      expect(seen.slice(0, 3)).toEqual(["spin Meko in Kiro: Running Meko's setup for Kiro (@yugabytedb/meko-mcp 9.9.9 from npm)", 'run', 'stop']);
      expect(seen.slice(3)).toEqual(code === 0 ? [] : ['say       Restart Kiro']);
    }
  });

  it('runs the setup on yes, or with --install-deps without asking but saying what it runs', async () => {
    const yes = step({ canAsk: true });
    expect(await yes.promise).toBe('set-up');
    expect(yes.calls).toContain('run');
    // Asked, it says nothing more: the question named what it runs, and its output streams.
    expect(yes.calls.some((c) => c.startsWith('say'))).toBe(false);
    const unattended = step({ installDeps: true });
    expect(await unattended.promise).toBe('set-up');
    expect(unattended.calls.some((c) => c.startsWith('ask'))).toBe(false);
    expect(unattended.calls).toContain("say     Running Meko's setup for Claude Code (@yugabytedb/meko-mcp 9.9.9 from npm)");
  });

  it("keeps the setup's output to itself when it works, and shows it when it fails", async () => {
    for (const code of [0, 1]) {
      const said: string[] = [];
      const done = ensureMekoSetup(
        { url: 'u', apiKey: 'k', canAsk: true, installDeps: false, noInstallDeps: false },
        {
          gaps: async () => ['x'], latest: async () => PKG, run: async () => code, output: () => ['Marketplace install skipped', 'Restart Claude Code'],
          ask: async () => 'y', say: (t) => said.push(t),
        },
      );
      // A gap still there fails the setup whatever its exit code, so its output is shown.
      await expect(done).rejects.toThrow(/didn't finish/);
      expect(said).toEqual(["    Running Meko's setup for Claude Code (@yugabytedb/meko-mcp 9.9.9 from npm)", '      Marketplace install skipped', '      Restart Claude Code']);
    }
    const said: string[] = [];
    let ran = false;
    await ensureMekoSetup(
      { url: 'u', apiKey: 'k', canAsk: true, installDeps: false, noInstallDeps: false },
      {
        gaps: async () => (ran ? [] : ['x']), latest: async () => PKG, run: async () => ((ran = true), 0), output: () => ['Marketplace install skipped'],
        ask: async () => 'y', say: (t) => said.push(t),
      },
    );
    expect(said).toEqual(["    Running Meko's setup for Claude Code (@yugabytedb/meko-mcp 9.9.9 from npm)"]);
  });

  it("names the local Meko's token in the command to run by hand", async () => {
    const step = ensureMekoSetup(
      { url: 'http://127.0.0.1:7778/mcp', apiKey: 'k', canAsk: false, installDeps: false, noInstallDeps: true, local: { home: '/home/u/.meko' } },
      { gaps: async () => ['x'], latest: async () => PKG, run: async () => 0, ask: async () => 'y', say: () => undefined },
    );
    await expect(step).rejects.toThrow('--url http://127.0.0.1:7778/mcp --api-key "$(MEKO_HOME=/home/u/.meko meko token)"');
  });

  it('fails when the setup exits non-zero or leaves something missing', async () => {
    await expect(step({ installDeps: true }, { code: 1 }).promise).rejects.toThrow(/didn't finish/);
    await expect(step({ installDeps: true }, { after: ['Claude Code has no meko MCP server.'] }).promise).rejects.toThrow(/Still: Claude Code has no meko MCP server\./);
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

  it("reports a Claude Code setup that leaves what Meko's capture needs missing as not finished", async () => {
    // A package that writes the URL but no key, and installs no plugin.
    const { fetchImpl } = fakePackage(`const url = process.argv[process.argv.indexOf('--url') + 1];
const d = process.env.CLAUDE_CONFIG_DIR;
writeFileSync(d + '/settings.json', JSON.stringify({ env: { MEKO_MCP_URL: url } }));`);
    const claudeDir = join(dir, 'claude');
    mkdirSync(claudeDir);
    const before = process.env.CLAUDE_CONFIG_DIR;
    process.env.CLAUDE_CONFIG_DIR = claudeDir;
    vi.stubGlobal('fetch', fetchImpl);
    try {
      const setUp = defaultMekoTools.setUpClaude('http://127.0.0.1:7778/mcp', 'local-token', () => undefined, 'the command');
      await expect(setUp).rejects.toThrow(/Still: .*Claude Code's settings have no MEKO_API_KEY\. Run it yourself: the command$/);
    } finally {
      vi.unstubAllGlobals();
      if (before === undefined) delete process.env.CLAUDE_CONFIG_DIR;
      else process.env.CLAUDE_CONFIG_DIR = before;
    }
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

  it('hands its output to log line by line when asked (the server has no terminal), and passes on the exit code', async () => {
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
