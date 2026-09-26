import { spawnSync, execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { chmodSync, existsSync, mkdirSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import pty from 'node-pty';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { tempDir } from '../helpers.js';

// Runs the real scripts/install.sh (up to --deps-only) against stand-in tools: a PATH with only
// the basic utilities it needs, plus stubs for uname, package managers, sudo, curl, gh and friends.
const SCRIPT = fileURLToPath(new URL('../../scripts/install.sh', import.meta.url));
const REAL = ['sh', 'bash', 'sed', 'tr', 'mktemp', 'grep', 'tar', 'gzip', 'mkdir', 'cp', 'chmod', 'mv', 'rm', 'cat', 'env', 'head', 'dirname'];
const GH_VERSION = '2.60.0';

let root: string;
let bin: string;
let repo: string;
let home: string;
let log: string;

function stub(dir: string, name: string, body: string) {
  writeFileSync(join(dir, name), `#!/bin/sh\n${body}\n`);
  chmodSync(join(dir, name), 0o755);
}

/** A tool as a package manager would install it: copied from the "repo" into bin. */
const installs = (from: string, to: string) => `for p in "$@"; do [ -f "${from}/$p" ] && cp "${from}/$p" "${to}/$p"; done; exit 0`;

beforeEach(() => {
  root = tempDir('akl-installsh-');
  bin = join(root, 'bin');
  repo = join(root, 'repo');
  home = join(root, 'home');
  log = join(root, 'calls.log');
  for (const d of [bin, repo, home]) mkdirSync(d, { recursive: true });
  for (const r of REAL) symlinkSync(execFileSync('sh', ['-c', `command -v ${r}`], { encoding: 'utf8' }).trim(), join(bin, r));
  const sum = spawnSync('sh', ['-c', 'command -v sha256sum']).status === 0 ? 'sha256sum' : 'shasum';
  symlinkSync(execFileSync('sh', ['-c', `command -v ${sum}`], { encoding: 'utf8' }).trim(), join(bin, sum));

  stub(bin, 'uname', 'case $1 in -s) echo "${FAKE_OS:-Linux}" ;; -m) echo "${FAKE_ARCH:-x86_64}" ;; esac');
  stub(bin, 'id', 'echo "${FAKE_UID:-1000}"');
  stub(bin, 'claude', `echo "claude $*" >> "${log}"; [ "$1 $2" = "auth status" ] && echo '{ "loggedIn": true }'; exit 0`);
  stub(bin, 'sysctl', 'echo 1');
  stub(bin, 'xcode-select', 'exit 0');
  stub(bin, 'sudo', `echo "sudo $*" >> "${log}"; [ "$1" = -n ] && shift; exec "$@"`);
  stub(bin, 'dnf', `echo "dnf $*" >> "${log}"; shift; ${installs(repo, bin)}`);
  stub(bin, 'brew', `echo "brew $*" >> "${log}"; case $1 in list) exit 1 ;; --prefix) echo /nowhere; exit 0 ;; esac; shift; ${installs(repo, bin)}`);

  // What the package managers hand out.
  stub(repo, 'tmux', 'echo "tmux ${FAKE_NEW_TMUX:-3.3a}"');
  stub(repo, 'git', 'exit 0');
  stub(repo, 'gh', 'exit 0');
  // curl serves gh's GitHub release from files in the repo dir.
  stub(repo, 'curl', `
out=""; url=""; effective=false
while [ $# -gt 0 ]; do
  case $1 in -o) out=$2; shift ;; -w) effective=true; shift ;; -*) ;; *) url=$1 ;; esac
  shift
done
echo "curl $url" >> "${log}"
if $effective; then echo "https://github.com/cli/cli/releases/tag/v${GH_VERSION}"; exit 0; fi
cp "${repo}/release/\${url##*/}" "$out"`);

  // gh's release: a tarball with bin/gh inside, and its checksums file.
  const name = `gh_${GH_VERSION}_linux_amd64`;
  mkdirSync(join(repo, 'release', name, 'bin'), { recursive: true });
  stub(join(repo, 'release', name, 'bin'), 'gh', 'exit 0');
  execFileSync('tar', ['-czf', `${name}.tar.gz`, name], { cwd: join(repo, 'release') });
  const digest = createHash('sha256').update(readFileSync(join(repo, 'release', `${name}.tar.gz`))).digest('hex');
  writeFileSync(join(repo, 'release', `gh_${GH_VERSION}_checksums.txt`), `${digest}  ${name}.tar.gz\n`);
});

afterEach(() => rmSync(root, { recursive: true, force: true }));

/** What the stubs were asked to do, leaving out the sign-in checks. */
const calls = () =>
  (existsSync(log) ? readFileSync(log, 'utf8').trim().split('\n') : []).filter((c) => c && c !== 'claude auth status --json');

/** Puts a repo tool on PATH as if it were already installed. */
const have = (...tools: string[]) => tools.forEach((t) => execFileSync('cp', [join(repo, t), join(bin, t)]));

function run(args: string[], env: Record<string, string> = {}) {
  const r = spawnSync(join(bin, 'sh'), [SCRIPT, '--deps-only', ...args], {
    env: { PATH: bin, HOME: home, LANG: 'C', ...env },
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  return { code: r.status, err: r.stderr, calls: calls() };
}

describe('install.sh: installing missing tools', () => {
  it('shows the plan and stops without --install-deps when there is no one to ask', () => {
    const r = run(['--non-interactive']);
    expect(r.code).toBe(1);
    expect(r.err).toContain('Missing');
    expect(r.err).toContain('sudo -n dnf install -y tmux git curl');
    expect(r.err).toContain('download gh from github.com/cli/cli releases into ~/.local/bin');
    expect(r.err).toMatch(/Rerun with --install-deps/);
    expect(r.calls).toEqual([]); // nothing was installed
  });

  it('installs tmux, git and curl with the package manager, and gh from its release, with --install-deps', () => {
    const r = run(['--non-interactive', '--install-deps']);
    expect(r.err).toContain('Installed');
    expect(r.code).toBe(0);
    expect(r.calls).toContain('sudo -n dnf install -y tmux git curl');
    expect(r.calls).toContain(`curl https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_amd64.tar.gz`);
    expect(existsSync(join(home, '.local', 'bin', 'gh'))).toBe(true);
  });

  it('runs the package manager directly as root, and refreshes apt first', () => {
    rmSync(join(bin, 'dnf'));
    stub(bin, 'apt-get', `echo "apt-get $*" >> "${log}"; [ "$1" = update ] && exit 0; shift; ${installs(repo, bin)}`);
    have('gh');
    const r = run(['--non-interactive', '--install-deps'], { FAKE_UID: '0' });
    expect(r.code).toBe(0);
    expect(r.calls).toEqual(['apt-get update', 'apt-get install -y tmux git curl']);
  });

  it('refuses a gh download whose checksum is wrong', () => {
    have('tmux', 'git', 'curl');
    writeFileSync(join(repo, 'release', `gh_${GH_VERSION}_checksums.txt`), `${'0'.repeat(64)}  gh_${GH_VERSION}_linux_amd64.tar.gz\n`);
    const r = run(['--non-interactive', '--install-deps']);
    expect(r.code).toBe(1);
    expect(r.err).toMatch(/couldn't install gh/);
    expect(existsSync(join(home, '.local', 'bin', 'gh'))).toBe(false);
  });

  it('says so when the installed tmux is still too old', () => {
    have('git', 'curl', 'gh');
    stub(bin, 'tmux', 'echo "tmux 1.8"');
    const r = run(['--non-interactive', '--install-deps'], { FAKE_NEW_TMUX: '1.8' });
    expect(r.err).toContain('tmux 2.7+ (found 1.8)');
    expect(r.code).toBe(1);
    expect(r.err).toMatch(/tmux 2\.7 or newer is still not on PATH/);
  });

  it('installs Claude Code with its own installer when it is missing', () => {
    have('tmux', 'git', 'curl', 'gh');
    execFileSync('mv', [join(bin, 'claude'), join(repo, 'claude')]);
    // Anthropic's installer copies claude from here.
    writeFileSync(join(repo, 'release', 'install.sh'), `mkdir -p "$HOME/.local/bin"; cp "${repo}/claude" "$HOME/.local/bin/claude"; echo "claude installer ran" >> "${log}"\n`);
    const plan = run(['--non-interactive']);
    expect(plan.code).toBe(1);
    expect(plan.err).toContain('curl -fsSL https://claude.ai/install.sh | bash');
    const r = run(['--non-interactive', '--install-deps']);
    expect(r.code).toBe(0);
    expect(r.calls).toEqual(['curl https://claude.ai/install.sh', 'claude installer ran']);
    expect(existsSync(join(home, '.local', 'bin', 'claude'))).toBe(true);
  });

  it('finds tools in ~/.local/bin even when it is not on PATH, and shows root commands plainly without sudo', () => {
    have('tmux', 'git', 'curl');
    mkdirSync(join(home, '.local', 'bin'), { recursive: true });
    execFileSync('mv', [join(bin, 'claude'), join(home, '.local', 'bin', 'claude')]);
    execFileSync('cp', [join(repo, 'gh'), join(home, '.local', 'bin', 'gh')]);
    expect(run(['--non-interactive']).code).toBe(0);
    rmSync(join(bin, 'sudo'));
    rmSync(join(bin, 'tmux'));
    const r = run(['--non-interactive', '--install-deps']);
    expect(r.code).toBe(1);
    expect(r.err).toContain('As root, run:');
    expect(r.err).toContain('dnf install -y tmux');
    expect(r.err).not.toContain('(as root) dnf');
  });

  it('uses Homebrew on a Mac', () => {
    have('git', 'curl');
    const r = run(['--non-interactive', '--install-deps'], { FAKE_OS: 'Darwin', FAKE_ARCH: 'arm64' });
    expect(r.code).toBe(0);
    expect(r.calls).toContain('brew install tmux gh');
    expect(r.calls.some((c) => c.startsWith('sudo'))).toBe(false);
  });

  it('asks for Homebrew when a Mac has none', () => {
    have('git', 'curl');
    rmSync(join(bin, 'brew'));
    const r = run(['--non-interactive', '--install-deps'], { FAKE_OS: 'Darwin', FAKE_ARCH: 'arm64' });
    expect(r.code).toBe(1);
    expect(r.err).toMatch(/Install Homebrew \(https:\/\/brew\.sh\)/);
  });

  it('installs nothing when every tool is there, and never with --no-install-deps', () => {
    have('tmux', 'git', 'curl', 'gh');
    const ok = run(['--non-interactive']);
    expect(ok.code).toBe(0);
    expect(ok.calls).toEqual([]);
    expect(ok.err).toContain('Tools');
    rmSync(join(bin, 'tmux'));
    const no = run(['--no-install-deps']);
    expect(no.code).toBe(1);
    expect(no.err).toMatch(/drop --no-install-deps/);
    expect(no.calls).toEqual([]);
  });

  /** Runs the script in a terminal, answering its question with `answer`. */
  function runInTerminal(answer: string, prompt = 'Install them now?'): Promise<{ code: number; out: string; calls: string[] }> {
    return new Promise((resolve) => {
      const p = pty.spawn(join(bin, 'sh'), [SCRIPT, '--deps-only'], { cols: 200, rows: 40, cwd: home, env: { PATH: bin, HOME: home, LANG: 'C' } });
      let out = '';
      let asked = false;
      p.onData((d) => {
        out += d;
        if (!asked && out.includes(prompt)) {
          asked = true;
          p.write(`${answer}\r`);
        }
      });
      p.onExit(({ exitCode }) => resolve({ code: exitCode, out, calls: calls() }));
    });
  }

  it('asks before installing anything, and installs on yes', async () => {
    have('gh');
    const r = await runInTerminal('y');
    expect(r.out).toContain('sudo dnf install -y tmux git curl');
    expect(r.code).toBe(0);
    expect(r.calls).toContain('sudo dnf install -y tmux git curl');
  });

  it('installs nothing on no', async () => {
    have('gh');
    const r = await runInTerminal('n');
    expect(r.code).toBe(1);
    expect(r.calls).toEqual([]);
    expect(r.out).toMatch(/Install them and run this installer again/);
  });

  it('offers to sign gh in to GitHub', async () => {
    have('tmux', 'git', 'curl');
    stub(bin, 'gh', `echo "gh $*" >> "${log}"; [ "$1 $2" = "auth login" ]`);
    const r = await runInTerminal('y', 'Sign in now (gh auth login)?');
    expect(r.code).toBe(0);
    expect(r.calls).toEqual(['gh auth status', 'gh auth login']);
  });

  it('offers to sign Claude Code in', async () => {
    have('tmux', 'git', 'curl', 'gh');
    stub(bin, 'claude', `echo "claude $*" >> "${log}"; [ "$1 $2" = "auth status" ] && echo '{ "loggedIn": false }'; exit 0`);
    const r = await runInTerminal('y', 'Sign in now (claude auth login)?');
    expect(r.code).toBe(0);
    expect(r.calls).toEqual(['claude auth login']);
  });
});
