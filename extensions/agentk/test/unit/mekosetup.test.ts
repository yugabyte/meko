import { createHash } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { tempDir } from '../helpers.js';
import {
  MekoSetupError, claudeHasMekoMcpAsync, claudeMekoTokens, ensureMekoSetup, envKeyGap, latestMekoSetup, manualMekoSetup, matchesIntegrity, mekoClaudeGaps, readClaudeMeko,
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
  const configPath = () => join(dir, 'claude.json');
  const register = (entry: unknown) => writeFileSync(configPath(), JSON.stringify({ mcpServers: { meko: entry } }));
  const read = () => readClaudeMeko(claudeDir(), configPath());
  const ready = (env: Record<string, string> = { MEKO_API_KEY: 'k' }) => {
    installPlugin();
    settings({ enabledPlugins: { 'meko-agent-skills@meko-agent-skills': true }, env });
  };
  const never = () => {
    throw new Error('asked claude mcp get although ~/.claude.json has the server');
  };

  it('lists the plugin, the key and the MCP server when none is there', () => {
    expect(mekoClaudeGaps(read(), CLOUD, () => false)).toEqual([
      "The meko-agent-skills plugin isn't installed in Claude Code.",
      "Claude Code's settings have no MEKO_API_KEY.",
      'Claude Code has no meko MCP server.',
    ]);
  });

  it('is empty when both places use this server, whatever tokens they hold', () => {
    ready({ MEKO_API_KEY: 'settings-token' });
    register({ type: 'http', url: `${CLOUD}/`, headers: { Authorization: 'Bearer server-token' } });
    expect(mekoClaudeGaps(read(), CLOUD, never)).toEqual([]);
    expect(claudeMekoTokens(read(), 'https://MCP.MekoData.ai/mcp')).toEqual([
      { place: 'settings', token: 'settings-token' },
      { place: 'server', token: 'server-token' },
    ]);
  });

  it('counts a disabled plugin, and never a key only in the environment', () => {
    installPlugin();
    settings({ enabledPlugins: { 'meko-agent-skills@meko-agent-skills': false } });
    process.env.MEKO_API_KEY = 'k'; // sessions don't inherit the installer's environment
    try {
      expect(mekoClaudeGaps(read(), CLOUD, () => true)).toEqual([
        "The meko-agent-skills plugin is installed in Claude Code but not enabled.",
        "Claude Code's settings have no MEKO_API_KEY.",
      ]);
    } finally {
      delete process.env.MEKO_API_KEY;
    }
  });

  it('names the other server when either place points elsewhere, by host only', () => {
    ready({ MEKO_API_KEY: 'k', MEKO_MCP_URL: 'https://user:secret@meko.example.com/mcp?x=1' });
    register({ type: 'http', url: 'https://other.example.org/mcp', headers: { Authorization: 'Bearer k' } });
    const gaps = mekoClaudeGaps(read(), CLOUD, never);
    expect(gaps).toEqual([
      "Claude Code's settings point at meko.example.com, not mcp.mekodata.ai.",
      "Claude Code's meko MCP server points at other.example.org, not mcp.mekodata.ai.",
    ]);
    expect(gaps.join(' ')).not.toMatch(/secret|user/);
    // Neither token is on this server, so neither is tried.
    expect(claudeMekoTokens(read(), CLOUD)).toEqual([]);
  });

  it('counts unset MEKO_MCP_URL as Meko Cloud, which the hooks then use', () => {
    ready();
    register({ type: 'http', url: OWN, headers: { Authorization: 'Bearer k' } });
    expect(mekoClaudeGaps(read(), OWN, never)).toEqual(["Claude Code's settings point at mcp.mekodata.ai, not meko.example.com."]);
  });

  it('reads the MCP server token from its header, expanding ${VAR}, and counts a missing one', () => {
    ready();
    register({ type: 'http', url: CLOUD, headers: { authorization: 'Bearer ${AKL_TEST_TOKEN}' } });
    process.env.AKL_TEST_TOKEN = 'from-env';
    try {
      expect(read().server).toEqual({ url: CLOUD, token: 'from-env' });
    } finally {
      delete process.env.AKL_TEST_TOKEN;
    }
    register({ type: 'http', url: CLOUD });
    expect(mekoClaudeGaps(read(), CLOUD, never)).toEqual(["Claude Code's meko MCP server sends no token."]);
  });

  it('expands ${VAR} with Claude Code\'s settings env over the environment, as a session sees it', () => {
    ready({ MEKO_API_KEY: 'from-settings' });
    register({ type: 'http', url: CLOUD, headers: { Authorization: 'Bearer ${MEKO_API_KEY}' } });
    process.env.MEKO_API_KEY = 'the-servers-own';
    try {
      expect(read().server).toEqual({ url: CLOUD, token: 'from-settings' });
    } finally {
      delete process.env.MEKO_API_KEY;
    }
  });

  it("doesn't call a token from a variable AgentK can't see missing, and doesn't try it", () => {
    ready();
    register({ type: 'http', url: CLOUD, headers: { Authorization: 'Bearer ${ONLY_IN_MY_PROFILE}' } });
    expect(read().server).toEqual({ url: CLOUD, token: null, unresolved: 'ONLY_IN_MY_PROFILE' });
    expect(mekoClaudeGaps(read(), CLOUD, never)).toEqual([]);
    expect(claudeMekoTokens(read(), CLOUD)).toEqual([{ place: 'settings', token: 'k' }]);
    // A default makes it resolved.
    register({ type: 'http', url: CLOUD, headers: { Authorization: 'Bearer ${ONLY_IN_MY_PROFILE:-fallback}' } });
    expect(read().server).toEqual({ url: CLOUD, token: 'fallback' });
  });

  it("doesn't count a meko MCP server Claude Code couldn't be asked about", () => {
    ready();
    expect(mekoClaudeGaps(read(), CLOUD, () => null)).toEqual([]);
    expect(mekoClaudeGaps(read(), CLOUD, () => false)).toEqual(['Claude Code has no meko MCP server.']);
  });

  it("asks [claude] command, telling 'not there' (exit 1) from 'couldn't ask'", async () => {
    const script = (name: string, body: string) => {
      const f = join(dir, name);
      writeFileSync(f, `#!/bin/sh\n${body}\n`, { mode: 0o755 });
      return f;
    };
    expect(await claudeHasMekoMcpAsync(script('has', '[ "$1 $2 $3" = "mcp get meko" ] && exit 0; exit 3'))).toBe(true);
    expect(await claudeHasMekoMcpAsync(script('hasnt', 'exit 1'))).toBe(false);
    expect(await claudeHasMekoMcpAsync(script('broken', 'exit 2'))).toBe(null);
    expect(await claudeHasMekoMcpAsync(join(dir, 'no-such-claude'))).toBe(null);
  });

  it("flags a different key in Claude Code's settings only when AgentK's key came from the installer's environment", () => {
    ready({ MEKO_API_KEY: 'other-account' });
    expect(envKeyGap(read(), 'from-env', true)).toEqual([
      "Claude Code's settings hold a different MEKO_API_KEY, which AgentK would use instead of this one once it runs as a service.",
    ]);
    expect(envKeyGap(read(), 'from-env', false)).toEqual([]);
    expect(envKeyGap(read(), 'other-account', true)).toEqual([]);
  });

  it('checks neither URL nor token of a server without a URL, and asks claude only when there is no entry', () => {
    ready();
    register({ type: 'stdio', command: 'meko-proxy' });
    expect(mekoClaudeGaps(read(), CLOUD, never)).toEqual([]);
    expect(claudeMekoTokens(read(), CLOUD)).toEqual([{ place: 'settings', token: 'k' }]);
    rmSync(configPath());
    expect(mekoClaudeGaps(read(), CLOUD, () => true)).toEqual([]);
  });

  it('compares URLs without a trailing slash and with the host in any case', () => {
    expect(sameMekoUrl('https://MCP.mekodata.ai/mcp/', CLOUD)).toBe(true);
    expect(sameMekoUrl('https://mcp.mekodata.ai/other', CLOUD)).toBe(false);
    expect(sameMekoUrl('http://mcp.mekodata.ai/mcp', CLOUD)).toBe(false);
  });
});

describe('the command to set up Meko by hand', () => {
  it("reads the local Meko's token in the user's shell, from the Meko AgentK uses", () => {
    expect(manualMekoSetup('http://127.0.0.1:7778/mcp')).toBe('npx @yugabytedb/meko-mcp --client claude-code --url http://127.0.0.1:7778/mcp --api-key "$(meko token)"');
    expect(manualMekoSetup('http://localhost:7778/mcp', 'kiro', '/home/u/.meko')).toBe(
      'npx @yugabytedb/meko-mcp --client kiro --url http://localhost:7778/mcp --api-key "$(MEKO_HOME=/home/u/.meko meko token)"',
    );
    expect(manualMekoSetup('http://[::1]:7778/mcp', 'claude-code', "/data/it's meko")).toMatch(/--api-key "\$\(MEKO_HOME='\/data\/it'\\''s meko' meko token\)"$/);
  });

  it('keeps the key off it for any other server', () => {
    expect(manualMekoSetup('https://mcp.mekodata.ai/mcp', 'claude-code', '/home/u/.meko')).toBe('npx @yugabytedb/meko-mcp --client claude-code --url https://mcp.mekodata.ai/mcp');
    expect(manualMekoSetup('not a url')).not.toContain('--api-key');
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

  it('asks with the package it will run, and stops when the answer is no', async () => {
    const { promise, calls } = step({ canAsk: true }, { answer: 'n' });
    await expect(promise).rejects.toBeInstanceOf(MekoSetupError);
    expect(calls).toEqual(['latest', "ask Run Meko's setup (@yugabytedb/meko-mcp 9.9.9 from npm)? (Y/n)"]);
  });

  it('says it in a few words, not the list of gaps', async () => {
    const warned: string[] = [];
    await ensureMekoSetup(
      { url: 'u', apiKey: 'k', canAsk: false, installDeps: false, noInstallDeps: true },
      { gaps: async () => ['Claude Code has no meko MCP server.'], latest: async () => PKG, run: async () => 0, ask: async () => 'y', say: () => undefined, warn: (l, t) => warned.push(`${l}: ${t}`) },
    ).catch(() => undefined);
    expect(warned).toEqual(['Meko in Claude: not set up for this Meko']);
  });

  it('runs the setup on yes, or with --install-deps without asking but saying what it runs', async () => {
    const yes = step({ canAsk: true });
    expect(await yes.promise).toBe('set-up');
    expect(yes.calls).toContain('run');
    const unattended = step({ installDeps: true });
    expect(await unattended.promise).toBe('set-up');
    expect(unattended.calls.some((c) => c.startsWith('ask'))).toBe(false);
    expect(unattended.calls).toContain("say     Running Meko's setup (@yugabytedb/meko-mcp 9.9.9 from npm)");
  });

  it("names the local Meko's token in the command to run by hand", async () => {
    const step = ensureMekoSetup(
      { url: 'http://127.0.0.1:7778/mcp', apiKey: 'k', canAsk: false, installDeps: false, noInstallDeps: true, mekoHome: '/home/u/.meko' },
      { gaps: async () => ['x'], latest: async () => PKG, run: async () => 0, ask: async () => 'y', say: () => undefined, warn: () => undefined },
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

  it("reports a Claude Code setup that leaves the meko MCP server with no token as not finished", async () => {
    // What the package does for the local Meko when it gets no key: the URL, and no Authorization header.
    const { fetchImpl } = fakePackage(`const url = process.argv[process.argv.indexOf('--url') + 1];
const d = process.env.CLAUDE_CONFIG_DIR;
writeFileSync(d + '/settings.json', JSON.stringify({ env: { MEKO_MCP_URL: url, MEKO_API_KEY: 'mko_tkn_cloud' } }));
writeFileSync(d + '/.claude.json', JSON.stringify({ mcpServers: { meko: { type: 'http', url } } }));`);
    const claudeDir = join(dir, 'claude');
    mkdirSync(claudeDir);
    const before = process.env.CLAUDE_CONFIG_DIR;
    process.env.CLAUDE_CONFIG_DIR = claudeDir;
    vi.stubGlobal('fetch', fetchImpl);
    try {
      const setUp = defaultMekoTools.setUpClaude('http://127.0.0.1:7778/mcp', 'local-token', () => undefined, 'claude', '/home/u/.meko');
      await expect(setUp).rejects.toThrow(/Still: .*Claude Code's meko MCP server sends no token\./);
      await expect(setUp).rejects.toThrow('Run it yourself: npx @yugabytedb/meko-mcp --client claude-code --url http://127.0.0.1:7778/mcp --api-key "$(MEKO_HOME=/home/u/.meko meko token)"');
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
