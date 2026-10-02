import { execFileSync } from 'node:child_process';
import { chmodSync, existsSync, mkdirSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  MIN_MOD_CLAUDE, claudeVersion, hooksSettingsPath, loginShellCommand, modDirPath, shq, versionAtLeast, writeHookFiles, writeLauncher,
  writeModFiles, type LaunchSpec,
} from '../../src/server/claude.js';
import { getPaths, type Paths } from '../../src/server/paths.js';
import { tmuxName } from '../../src/server/tmux.js';
import { tempDir } from '../helpers.js';

describe('the hooks settings file', () => {
  it('allows editing designs and publishing Artifacts, with Edit() rules only', () => {
    const paths = getPaths(join(tempDir('akl-hooks-'), 'ak'));
    writeHookFiles(paths);
    const allow: string[] = JSON.parse(readFileSync(hooksSettingsPath(paths), 'utf8')).permissions.allow;
    // Claude Code ignores Write() rules and warns about them at every session start; Edit() covers Write.
    expect(allow).toEqual([`Edit(/${paths.designs}/**)`, 'Artifact']);
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

describe('tmuxName', () => {
  it('keeps two projects on one repo apart', () => {
    expect(tmuxName('api', 'v2-fix')).not.toBe(tmuxName('api-v2', 'fix'));
    expect(tmuxName('demo.repo', 'a:b')).toBe('ak-demo-repo--a-b');
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
for v in "$PWD" "\${PROFILE_RAN-unset}" "\${MEKO_AGENT_ID-unset}" "\${MEKO_CHECKPOINT_INTERVAL-unset}" "\${AK_HOME-unset}" "\${CLAUDECODE-unset}" "\${AK_TRICKY-unset}"; do printf '%s\\0' "$v"; done > ${shq(out + '.env')}
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
    const script = writeLauncher(paths, s);
    execFileSync('sh', [script], { env: { ...process.env, HOME: home, AK_HOME: '/live/install', CLAUDECODE: '1' }, stdio: 'ignore' });
    const split = (f: string) => readFileSync(f, 'utf8').split('\0').slice(0, -1);
    return { args: split(out + '.args'), env: split(out + '.env') };
  }

  it('passes every value through intact, after "--" for the prompt, with the session variables set after the profile', () => {
    const s = spec({ prompt: '---\ntitle: front matter\n---\nDo it; echo $HOME' });
    const { args, env } = run(s);
    expect(args).toEqual([
      '--settings', join(paths.run, 'hooks.json'), '--session-id', 'c0ffee', '--name', 'Fix "it" now',
      '--add-dir', s.addDirs[0], '--append-system-prompt', s.appendSystemPrompt, '--', s.prompt,
    ]);
    expect(env).toEqual([s.worktree, 'yes', 'claude_code:demo', 'unset', 'unset', 'unset', s.env.AK_TRICKY]);
  });

  it('loads each plugin folder with --plugin-dir, after the added directories', () => {
    const s = spec({ pluginDirs: ['/ak/run/mod/agentk'] });
    expect(run(s).args).toEqual([
      '--settings', join(paths.run, 'hooks.json'), '--session-id', 'c0ffee', '--name', 'Fix "it" now',
      '--add-dir', s.addDirs[0], '--plugin-dir', '/ak/run/mod/agentk', '--append-system-prompt', s.appendSystemPrompt,
    ]);
  });

  it('keeps a variable it sets even when it is also listed to unset', () => {
    const { env } = run(spec({ resume: true, env: { MEKO_AGENT_ID: 'x', MEKO_CHECKPOINT_INTERVAL: '2147483' } }));
    expect(env.slice(2, 4)).toEqual(['x', '2147483']);
  });

  it('finds tools in ~/.local/bin (where the installer puts Claude Code and gh) even when the profile leaves it out', () => {
    run(spec({}));
    expect(readFileSync(out + '.path', 'utf8').split(':')).toContain(join(home, '.local', 'bin'));
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

  it('is skipped when its source is missing from the install', () => {
    const paths = getPaths(join(home, 'ak'));
    expect(writeModFiles(paths, join(home, 'nowhere'))).toBe(false);
  });

  it('needs Claude Code 2.1.286 or later, comparing versions part by part', () => {
    expect(MIN_MOD_CLAUDE).toBe('2.1.286');
    expect(versionAtLeast('2.1.286', MIN_MOD_CLAUDE)).toBe(true);
    expect(versionAtLeast('2.1.287', MIN_MOD_CLAUDE)).toBe(true);
    expect(versionAtLeast('2.2.0', MIN_MOD_CLAUDE)).toBe(true);
    expect(versionAtLeast('3.0.1', MIN_MOD_CLAUDE)).toBe(true);
    expect(versionAtLeast('2.1.285', MIN_MOD_CLAUDE)).toBe(false);
    expect(versionAtLeast('2.1.29', MIN_MOD_CLAUDE)).toBe(false);
    expect(versionAtLeast('1.9.999', MIN_MOD_CLAUDE)).toBe(false);
  });

  it("reads the version Claude Code prints, and null when it can't run", async () => {
    const bin = join(home, 'claude');
    writeFileSync(bin, '#!/bin/sh\necho "2.1.290 (Claude Code)"\n');
    chmodSync(bin, 0o755);
    expect(await claudeVersion(bin)).toBe('2.1.290');
    writeFileSync(bin, '#!/bin/sh\necho nope; exit 1\n');
    expect(await claudeVersion(bin)).toBeNull();
    expect(await claudeVersion(join(home, 'missing'))).toBeNull();
  });
});
