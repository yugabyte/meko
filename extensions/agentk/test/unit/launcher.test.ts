import { execFileSync } from 'node:child_process';
import { chmodSync, existsSync, mkdirSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { homedir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  claudeLauncher, hooksSettingsPath, modDirPath, writeHookFiles, writeModFiles, type LaunchSpec,
} from '../../src/server/claude.js';
import { versionAtLeast } from '../../src/shared/harnesses.js';
import { cliVersion } from '../../src/server/harness/version.js';
import { adoptLoginPath, loginShellCommand, shq, writeLauncher } from '../../src/server/launcher.js';
import { getPaths, type Paths } from '../../src/server/paths.js';
import { paneName } from '../../src/server/hostclient.js';
import { tempDir } from '../helpers.js';

describe('the hooks settings file', () => {
  it('allows editing designs, publishing Artifacts, reading pasted images and listing sessions with the session tools, with Edit() rules only for edits', () => {
    const paths = getPaths(join(tempDir('akl-hooks-'), 'ak'));
    writeHookFiles(paths);
    const allow: string[] = JSON.parse(readFileSync(hooksSettingsPath(paths), 'utf8')).permissions.allow;
    // Claude Code ignores Write() rules and warns about them at every session start; Edit() covers Write.
    // The session tools that change something ask first.
    expect(allow).toEqual([`Edit(/${paths.designs}/**)`, 'Artifact', `Read(/${paths.pastes}/**)`, 'mcp__agentk_sessions__list']);
  });

  it('turns off the plugins it is given, and leaves plugins alone otherwise', () => {
    const paths = getPaths(join(tempDir('akl-hooks-'), 'ak'));
    writeHookFiles(paths);
    expect(JSON.parse(readFileSync(hooksSettingsPath(paths), 'utf8')).enabledPlugins).toBeUndefined();
    writeHookFiles(paths, { disabledPlugins: ['meko-agent-skills@meko-agent-skills', 'meko-agent-skills@fork'] });
    expect(JSON.parse(readFileSync(hooksSettingsPath(paths), 'utf8')).enabledPlugins)
      .toEqual({ 'meko-agent-skills@meko-agent-skills': false, 'meko-agent-skills@fork': false });
  });
});

describe('the server taking PATH from the login shell', () => {
  afterEach(() => vi.unstubAllEnvs());

  // launchd's PATH to start from; Homebrew's folders are added whatever the shell does. macOS's
  // first run of a new script can take a few hundred ms, so only the slow shell gets a short limit.
  it.each([
    ['prints its PATH', "printf '/opt/x/bin\\0\\0'", 5000, null, '/opt/x/bin'],
    ['prints no PATH', "printf '\\0\\0'", 5000, 'it printed no PATH', '/usr/bin:/bin'],
    ['fails', 'exit 3', 5000, 'it exited with code 3', '/usr/bin:/bin'],
    ['takes too long', 'exec sleep 5', 500, 'it took longer than 0.5 s', '/usr/bin:/bin'],
  ])('when the shell %s', (_, body, timeoutMs, why, path) => {
    vi.stubEnv('PATH', '/usr/bin:/bin');
    const shell = join(tempDir('akl-shell-'), 'sh');
    writeFileSync(shell, `#!/bin/sh\n${body}\n`, { mode: 0o755 });
    expect(adoptLoginPath(shell, timeoutMs)).toBe(why);
    expect(process.env.PATH).toBe(`${path}:${join(homedir(), '.local', 'bin')}:/usr/local/bin:/opt/homebrew/bin`);
  });
});

describe('loginShellCommand', () => {
  const script = '/home/me/.agent-k-local/run/demo--fix-it.sh';

  it('runs the second half through POSIX login shells with POSIX quoting', () => {
    for (const sh of ['/bin/bash', '/usr/bin/zsh', '/bin/ksh', '/bin/dash', '/bin/sh', '/opt/homebrew/bin/bash']) {
      expect(loginShellCommand(sh, script)).toEqual([sh, `exec /bin/sh ${shq(script)} --in-login-shell`]);
    }
  });

  it('gives fish only a path that needs no quoting, and falls back to /bin/sh for anything else', () => {
    expect(loginShellCommand('/usr/bin/fish', script)).toEqual(['/usr/bin/fish', `exec /bin/sh ${script} --in-login-shell`]);
    // fish reads backslashes in single quotes; tcsh and csh take no -l with -c; unknown shells may parse anything.
    const odd = "/home/o'brien/a b\\c/run/x.sh";
    expect(loginShellCommand('/usr/bin/fish', odd)).toEqual(['/bin/sh', `exec /bin/sh ${shq(odd)} --in-login-shell`]);
    expect(loginShellCommand('/bin/tcsh', script)[0]).toBe('/bin/sh');
    expect(loginShellCommand('/usr/bin/nu', script)[0]).toBe('/bin/sh');
  });
});

describe('paneName', () => {
  it('keeps two projects on one repo apart', () => {
    expect(paneName('api', 'v2-fix')).not.toBe(paneName('api-v2', 'fix'));
    expect(paneName('demo.repo', 'a:b')).toBe('ak-demo-repo--a-b');
  });
});

describe('writeLauncher (run through the login shell)', () => {
  let home: string;
  let paths: Paths;
  let out: string;
  beforeEach(() => {
    home = tempDir('akl-launcher-');
    paths = getPaths(join(home, 'ak'));
    out = join(home, 'out');
    // A profile that exports its own Meko values: the session's must win.
    const profile = 'export PROFILE_RAN=yes MEKO_AGENT_ID=from-profile MEKO_CHECKPOINT_INTERVAL=1\n';
    for (const f of ['.profile', '.bash_profile', '.zprofile']) writeFileSync(join(home, f), profile);
  });
  afterEach(() => rmSync(home, { recursive: true, force: true }));

  function spec(over: Partial<LaunchSpec>): LaunchSpec {
    // Stand-in for claude: records its arguments, cwd and the variables that matter, NUL-separated.
    const fake = join(home, 'fake claude');
    writeFileSync(fake, `#!/bin/sh
for a in "$@"; do printf '%s\\0' "$a"; done > ${shq(out + '.args')}
printf '%s' "$PATH" > ${shq(out + '.path')}
for v in "$PWD" "\${PROFILE_RAN-unset}" "\${MEKO_AGENT_ID-unset}" "\${MEKO_CHECKPOINT_INTERVAL-unset}" "\${AK_HOME-unset}" "\${CLAUDECODE-unset}" "\${KIRO_SESSION_ID-unset}" "\${AK_TRICKY-unset}"; do printf '%s\\0' "$v"; done > ${shq(out + '.env')}
`);
    chmodSync(fake, 0o755);
    const worktree = join(home, "it's a \"worktree\" $HOME `x`");
    mkdirSync(worktree, { recursive: true });
    return {
      sessionId: 'demo--fix-it', worktree, claudeCommand: fake, claudeSessionId: 'c0ffee', resume: false,
      name: 'Fix "it" now', planMode: false, addDirs: [join(home, 'designs', 'demo--fix-it')],
      appendSystemPrompt: "Line one\nLine 'two' $(rm -rf /) `id` \\n", prompt: null,
      env: { MEKO_AGENT_ID: 'claude_code:demo', AK_TRICKY: "a'b\"c$d`e`\\f\ng" }, unsetEnv: ['MEKO_CHECKPOINT_INTERVAL'],
      ...over,
    };
  }

  function run(s: LaunchSpec): { args: string[]; env: string[] } {
    const script = writeLauncher(paths, claudeLauncher(paths, s));
    // Started from inside Claude Code or Kiro: neither's markers reach the session.
    execFileSync('sh', [script], { env: { ...process.env, HOME: home, AK_HOME: '/live/install', CLAUDECODE: '1', KIRO_SESSION_ID: 'k-1' }, stdio: 'ignore' });
    const split = (f: string) => readFileSync(f, 'utf8').split('\0').slice(0, -1);
    return { args: split(out + '.args'), env: split(out + '.env') };
  }

  it('passes every value through intact, plugin folders and MCP configs after the added ones and the prompt after "--", with the session variables set after the profile', () => {
    const s = spec({
      prompt: '---\ntitle: front matter\n---\nDo it; echo $HOME', pluginDirs: ['/ak/run/mod/agentk', '/ak/run/tools/claude'], mcpConfigs: ['/ak/run/tools/mcp.json'],
    });
    const { args, env } = run(s);
    expect(args).toEqual([
      '--settings', join(paths.run, 'hooks.json'), '--session-id', 'c0ffee', '--name', 'Fix "it" now',
      '--add-dir', s.addDirs[0], '--plugin-dir', '/ak/run/mod/agentk', '--plugin-dir', '/ak/run/tools/claude', '--mcp-config', '/ak/run/tools/mcp.json',
      '--append-system-prompt', s.appendSystemPrompt, '--', s.prompt,
    ]);
    expect(env).toEqual([s.worktree, 'yes', 'claude_code:demo', 'unset', 'unset', 'unset', 'unset', s.env.AK_TRICKY]);
    // ~/.local/bin is where the installer puts Claude Code and gh, even when the profile leaves it out.
    expect(readFileSync(out + '.path', 'utf8').split(':')).toContain(join(home, '.local', 'bin'));
  });

  it('keeps a variable it sets even when it is also listed to unset', () => {
    const { env } = run(spec({ resume: true, env: { MEKO_AGENT_ID: 'x', MEKO_CHECKPOINT_INTERVAL: '2147483' } }));
    expect(env.slice(2, 4)).toEqual(['x', '2147483']);
  });
});

describe("AgentK's mod", () => {
  let home: string;
  beforeEach(() => {
    home = tempDir('akl-mod-');
  });
  afterEach(() => {
    rmSync(home, { recursive: true, force: true });
  });

  it('is copied to the run folder from the repo, without its tests', () => {
    const paths = getPaths(join(home, 'ak'));
    expect(writeModFiles(paths)).toBe(true);
    const dir = modDirPath(paths);
    expect(dir).toBe(join(paths.run, 'mod', 'agentk'));
    expect(JSON.parse(readFileSync(join(dir, '.claude-plugin', 'plugin.json'), 'utf8')).name).toBe('agentk');
    expect(JSON.parse(readFileSync(join(dir, 'hooks', 'hooks.json'), 'utf8'))).toEqual({ modules: ['./register.ts'] });
    expect(readFileSync(join(dir, 'hooks', 'register.ts'), 'utf8')).toMatch(/\/mod\/poll/);
    expect(existsSync(join(dir, 'tests'))).toBe(false);
  });

  it('writes only the files that changed (Claude reloads the mod on every write), and none left half-written', () => {
    const paths = getPaths(join(home, 'ak'));
    const src = join(home, 'src');
    const put = (f: string, text: string) => {
      mkdirSync(join(src, f, '..'), { recursive: true });
      writeFileSync(join(src, f), text);
    };
    put('.claude-plugin/plugin.json', '{"name":"agentk"}');
    put('hooks/hooks.json', '{"modules":["./register.ts"]}');
    put('hooks/register.ts', 'export const register = () => {}\n');
    expect(writeModFiles(paths, src)).toBe(true);
    const dir = modDirPath(paths);
    const mtime = (f: string) => statSync(join(dir, f)).mtimeMs;
    const before = { manifest: mtime('.claude-plugin/plugin.json'), module: mtime('hooks/register.ts') };
    execFileSync('sleep', ['0.05']);
    put('hooks/register.ts', 'export const register = (on) => {}\n');
    expect(writeModFiles(paths, src)).toBe(true);
    expect(readFileSync(join(dir, 'hooks', 'register.ts'), 'utf8')).toBe('export const register = (on) => {}\n');
    expect(mtime('hooks/register.ts')).not.toBe(before.module);
    expect(mtime('.claude-plugin/plugin.json')).toBe(before.manifest);
    expect(readdirSync(join(dir, 'hooks')).sort()).toEqual(['hooks.json', 'register.ts']);
  });

  it('is removed when its source is missing from the install, rather than an old copy kept', () => {
    const paths = getPaths(join(home, 'ak'));
    expect(writeModFiles(paths)).toBe(true);
    expect(existsSync(modDirPath(paths))).toBe(true);
    expect(writeModFiles(paths, join(home, 'nowhere'))).toBe(false);
    expect(existsSync(modDirPath(paths))).toBe(false);
  });

  it('writes nothing when one of its files is missing, and removes a copy a failed write left', () => {
    const paths = getPaths(join(home, 'ak'));
    const src = join(home, 'src');
    mkdirSync(join(src, '.claude-plugin'), { recursive: true });
    writeFileSync(join(src, '.claude-plugin', 'plugin.json'), '{"name":"agentk"}');
    expect(writeModFiles(paths, src)).toBe(false); // no hooks/: nothing half-written
    expect(existsSync(modDirPath(paths))).toBe(false);
    mkdirSync(join(src, 'hooks'));
    writeFileSync(join(src, 'hooks', 'hooks.json'), '{}');
    writeFileSync(join(src, 'hooks', 'register.ts'), '');
    // A file where the hooks folder should be: the write fails, and the copy is removed.
    mkdirSync(modDirPath(paths), { recursive: true });
    writeFileSync(join(modDirPath(paths), 'hooks'), 'in the way');
    expect(() => writeModFiles(paths, src)).toThrow();
    expect(existsSync(modDirPath(paths))).toBe(false);
  });

  it('compares Claude Code versions part by part', () => {
    expect(versionAtLeast('2.1.286', '2.1.287')).toBe(false);
    expect(versionAtLeast('2.1.287', '2.1.287')).toBe(true);
    expect(versionAtLeast('2.2.0', '2.1.287')).toBe(true);
    expect(versionAtLeast('3.0.1', '2.1.287')).toBe(true);
    expect(versionAtLeast('2.1.29', '2.1.287')).toBe(false);
    expect(versionAtLeast('1.9.999', '2.1.287')).toBe(false);
  });

  it("reads the version Claude Code prints, and null when it can't run", async () => {
    const bin = join(home, 'claude');
    writeFileSync(bin, '#!/bin/sh\necho "2.1.290 (Claude Code)"\n');
    chmodSync(bin, 0o755);
    expect(await cliVersion(bin)).toBe('2.1.290');
    writeFileSync(bin, '#!/bin/sh\necho nope; exit 1\n');
    expect(await cliVersion(bin)).toBeNull();
    expect(await cliVersion(join(home, 'missing'))).toBeNull();
  });
});
