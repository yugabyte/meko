import { spawnSync, execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { chmodSync, existsSync, mkdirSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import pty from 'node-pty';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { tempDir } from '../helpers.js';

// Runs the real scripts/deps.sh (the prerequisites `me_koder install` checks first) against stand-in
// tools: a PATH with only the basic utilities it needs, plus stubs for uname, package managers, sudo,
// curl, gh and friends.
const SCRIPT = fileURLToPath(new URL('../../scripts/deps.sh', import.meta.url));
const STUB = fileURLToPath(new URL('../../scripts/install.sh', import.meta.url));
const REAL = ['sh', 'bash', 'sed', 'tr', 'mktemp', 'grep', 'tar', 'gzip', 'mkdir', 'cp', 'chmod', 'mv', 'rm', 'cat', 'env', 'head', 'dirname', 'ln', 'readlink', 'awk'];
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

function run(args: string[], env: Record<string, string> = {}, script = SCRIPT) {
  const r = spawnSync(join(bin, 'sh'), [script, ...args], {
    env: { PATH: bin, HOME: home, LANG: 'C', AK_BREW_DIRS: '', ...env },
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  return { code: r.status, err: r.stderr, calls: calls() };
}

describe('deps.sh: installing missing tools', () => {
  it('shows the plan and stops without --install-deps when there is no one to ask', () => {
    const r = run(['--non-interactive']);
    expect(r.code).toBe(1);
    expect(r.err).toContain('Missing');
    expect(r.err).toContain('sudo -n dnf install -y git curl');
    // gh is optional: not in the plan without --install-deps.
    expect(r.err).not.toContain('download gh');
    expect(r.err).toContain('Run meko extension install agentk -- --install-deps to install them');
    expect(r.calls).toEqual([]); // nothing was installed
  });

  it('installs git and curl with the package manager, and gh from its release, with --install-deps', () => {
    const r = run(['--non-interactive', '--install-deps']);
    expect(r.err).toContain('Installed');
    expect(r.code).toBe(0);
    expect(r.calls).toContain('sudo -n dnf install -y git curl');
    expect(r.calls).toContain(`curl https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_amd64.tar.gz`);
    expect(existsSync(join(home, '.local', 'bin', 'gh'))).toBe(true);
  });

  it('runs the package manager directly as root, and refreshes apt first', () => {
    rmSync(join(bin, 'dnf'));
    stub(bin, 'apt-get', `echo "apt-get $*" >> "${log}"; [ "$1" = update ] && exit 0; shift; ${installs(repo, bin)}`);
    have('gh');
    const r = run(['--non-interactive', '--install-deps'], { FAKE_UID: '0' });
    expect(r.code).toBe(0);
    expect(r.calls).toEqual(['apt-get update', 'apt-get install -y git curl']);
    // The plan shows the command exactly as it runs.
    expect(r.err).toContain('apt-get update && env DEBIAN_FRONTEND=noninteractive apt-get install -y git curl');
  });

  it('refuses a gh download whose checksum is wrong', () => {
    have('git', 'curl');
    writeFileSync(join(repo, 'release', `gh_${GH_VERSION}_checksums.txt`), `${'0'.repeat(64)}  gh_${GH_VERSION}_linux_amd64.tar.gz\n`);
    const r = run(['--non-interactive', '--install-deps']);
    expect(r.code).toBe(1);
    expect(r.err).toMatch(/couldn't install gh/);
    expect(existsSync(join(home, '.local', 'bin', 'gh'))).toBe(false);
  });

  it('installs Claude Code with its own installer when it is missing', () => {
    have('git', 'curl', 'gh');
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

  it('installs Kiro CLI instead with --agent kiro, and no Claude Code with it', () => {
    stub(bin, 'unzip', 'exit 0');
    have('git', 'curl', 'gh');
    rmSync(join(bin, 'claude'));
    stub(repo, 'kiro-cli', `echo "kiro-cli $*" >> "${log}"; exit 0`);
    writeFileSync(join(repo, 'release', 'install'), `mkdir -p "$HOME/.local/bin"; cp "${repo}/kiro-cli" "$HOME/.local/bin/kiro-cli"; echo "kiro installer ran" >> "${log}"\n`);
    const plan = run(['--non-interactive']);
    expect(plan.code).toBe(1);
    expect(plan.err).toContain('missing: Claude Code (or OpenCode or Kiro CLI, with --agent opencode or --agent kiro)');
    const r = run(['--non-interactive', '--install-deps', '--agent', 'kiro']);
    expect(r.code).toBe(0);
    expect(r.calls).toEqual(['curl https://cli.kiro.dev/install', 'kiro installer ran', 'kiro-cli whoami']);
    expect(r.err).toMatch(/Tools\s+git, curl, gh, Kiro/);
  });

  it('finds tools in ~/.local/bin even when it is not on PATH, and shows root commands plainly without sudo', () => {
    have('git', 'curl');
    mkdirSync(join(home, '.local', 'bin'), { recursive: true });
    execFileSync('mv', [join(bin, 'claude'), join(home, '.local', 'bin', 'claude')]);
    execFileSync('cp', [join(repo, 'gh'), join(home, '.local', 'bin', 'gh')]);
    expect(run(['--non-interactive']).code).toBe(0);
    rmSync(join(bin, 'sudo'));
    rmSync(join(bin, 'git'));
    const r = run(['--non-interactive', '--install-deps']);
    expect(r.code).toBe(1);
    expect(r.err).toContain('As root, run:');
    expect(r.err).toContain('dnf install -y git');
    expect(r.err).not.toContain('(as root) dnf');
  });

  // A brew that installs what it can from the repo, then exits with an error (as on a macOS
  // Homebrew builds from source for).
  const failingBrew = () =>
    stub(bin, 'brew', `echo "brew $*" >> "${log}"; case $1 in list) exit 1 ;; --prefix) echo /nowhere; exit 0 ;; esac; shift; ${installs(repo, bin).replace(/exit 0$/, 'exit 1')}`);

  it('goes on when Homebrew exits with an error but installed the tools anyway', () => {
    have('git');
    failingBrew();
    const r = run(['--non-interactive', '--install-deps'], { FAKE_OS: 'Darwin', FAKE_ARCH: 'arm64' });
    expect(r.err).toContain('brew          exited with an error, but curl gh work: going on');
    expect(r.code).toBe(0);
  });

  it('names the tool Homebrew failed to install, and how to see why', () => {
    have('git');
    // curl lands, gh doesn't.
    rmSync(join(repo, 'gh'));
    failingBrew();
    const r = run(['--non-interactive', '--install-deps'], { FAKE_OS: 'Darwin', FAKE_ARCH: 'arm64' });
    expect(r.code).toBe(1);
    expect(r.err).toContain('brew failed (see the output above');
    expect(r.err).toContain('gh is still not available after installing it');
    expect(r.err).toContain("Run 'brew install gh' yourself to see what went wrong");
    expect(r.err).not.toContain('going on');
  });

  it('stops when the Linux package manager exits with an error, even if the tools landed', () => {
    have('gh');
    // A failed dnf or apt-get can leave the package database half-configured.
    stub(bin, 'dnf', `echo "dnf $*" >> "${log}"; shift; ${installs(repo, bin).replace(/exit 0$/, 'exit 1')}`);
    const r = run(['--non-interactive', '--install-deps']);
    expect(r.code).toBe(1);
    expect(r.err).toContain('installing git curl failed (see the output above). Install them yourself and run meko extension install agentk again.');
    expect(r.err).not.toContain('going on');
  });

  it('uses Homebrew on a Mac, finding it outside PATH without garbling its own output', () => {
    have('git', 'curl');
    // brew in its usual folder but not on PATH, as in a fresh shell on a Mac.
    const brewDir = join(root, 'homebrew', 'bin');
    mkdirSync(brewDir, { recursive: true });
    execFileSync('mv', [join(bin, 'brew'), join(brewDir, 'brew')]);
    const r = run(['--non-interactive', '--install-deps'], { FAKE_OS: 'Darwin', FAKE_ARCH: 'arm64', AK_BREW_DIRS: brewDir });
    expect(r.code).toBe(0);
    expect(r.calls).toContain('brew install gh');
    expect(r.calls.some((c) => c.startsWith('sudo'))).toBe(false);
    // The folder it looked in never leaks into the messages (it once shared a name with a color code).
    expect(r.err).not.toContain(brewDir);
  });

  it('goes on without gh, which AgentK asks for later, unless told to install it', () => {
    have('git', 'curl');
    for (const args of [['--non-interactive'], ['--no-install-deps']]) {
      const r = run(args);
      expect(r.code).toBe(0);
      expect(r.err).toContain("gh isn't installed: AgentK asks for it when you open a PR");
      expect(r.calls).toEqual([]);
    }
    const installed = run(['--non-interactive', '--install-deps']);
    expect(installed.code).toBe(0);
    expect(existsSync(join(home, '.local', 'bin', 'gh'))).toBe(true);
  });

  it('installs gh alone with --only gh, without root and without the other checks', () => {
    have('curl');
    rmSync(join(bin, 'claude'));
    expect(run(['--only', 'gh', '--non-interactive']).code).toBe(1);
    const r = run(['--only', 'gh', '--install-deps', '--non-interactive']);
    expect(r.code).toBe(0);
    expect(r.calls.every((c) => c.startsWith('curl '))).toBe(true);
    expect(existsSync(join(home, '.local', 'bin', 'gh'))).toBe(true);
    // There now: nothing to do.
    expect(run(['--only', 'gh', '--non-interactive']).code).toBe(0);
  });

  it('installs gh alone with Homebrew on a Mac, and says where to get it without Homebrew', () => {
    const mac = { FAKE_OS: 'Darwin', FAKE_ARCH: 'arm64' };
    const r = run(['--only', 'gh', '--install-deps', '--non-interactive'], mac);
    expect(r.code).toBe(0);
    expect(r.calls).toEqual(['brew install gh']);
    rmSync(join(bin, 'gh'));
    rmSync(join(bin, 'brew'));
    const none = run(['--only', 'gh', '--install-deps', '--non-interactive'], mac);
    expect(none.code).toBe(1);
    expect(none.err).toContain('https://cli.github.com');
  });

  it('asks for Homebrew when a Mac has none', () => {
    have('git', 'curl');
    rmSync(join(bin, 'brew'));
    const r = run(['--non-interactive', '--install-deps'], { FAKE_OS: 'Darwin', FAKE_ARCH: 'arm64' });
    expect(r.code).toBe(1);
    expect(r.err).toMatch(/Install Homebrew \(https:\/\/brew\.sh\)/);
  });

  it('installs nothing when every tool is there (tmux isn\'t one), and never with --no-install-deps', () => {
    have('git', 'curl', 'gh');
    const ok = run(['--non-interactive']);
    expect(ok.code).toBe(0);
    expect(ok.calls).toEqual([]);
    expect(ok.err).toContain('Tools');
    expect(ok.err).not.toMatch(/tmux/i);
    rmSync(join(bin, 'git'));
    const no = run(['--no-install-deps']);
    expect(no.code).toBe(1);
    expect(no.err).toMatch(/drop --no-install-deps/);
    expect(no.calls).toEqual([]);
  });

  /** Runs the script in a terminal, answering its question with `answer` (and then the `more` questions, in order). */
  function runInTerminal(answer: string, prompt = 'Install them now?', more: [string, string][] = []): Promise<{ code: number; out: string; calls: string[] }> {
    return new Promise((resolve) => {
      // Once the script ends, a shell keeps the terminal open until its last output is read: a pty
      // whose program has exited can lose what it wrote just before.
      const wrap = 'sh "$0"; s=$?; printf "\\n[exited %s]" "$s"; read -r _; exit "$s"';
      const p = pty.spawn(join(bin, 'sh'), ['-c', wrap, SCRIPT], { cols: 200, rows: 40, cwd: home, env: { PATH: bin, HOME: home, LANG: 'C', AK_BREW_DIRS: '' } });
      let out = '';
      const questions: [string, string][] = [[prompt, answer], ...more];
      let seen = 0;
      let ended = false;
      p.onData((d) => {
        out += d;
        const next = questions[0];
        // Each question is looked for in what came after the last one answered.
        if (next && out.indexOf(next[0], seen) >= 0) {
          seen = out.indexOf(next[0], seen) + next[0].length;
          questions.shift();
          p.write(`${next[1]}\r`);
        }
        if (!ended && /\[exited \d+\]/.test(out)) {
          ended = true;
          p.write('\r');
        }
      });
      p.onExit(({ exitCode }) => resolve({ code: exitCode, out, calls: calls() }));
    });
  }

  it('asks before installing anything, and installs on yes, then offers gh on its own', async () => {
    const r = await runInTerminal('y', 'Install them now?', [['Install gh, GitHub', '']]);
    expect(r.out).toContain('sudo dnf install -y git curl');
    // gh isn't in the main plan: it's shown with its own question.
    const [main, gh] = r.out.split('Install them now?');
    expect(main).not.toContain('download gh');
    expect(gh.split('Install gh, GitHub')[0]).toContain('download gh from github.com/cli/cli releases into ~/.local/bin');
    expect(r.code).toBe(0);
    expect(r.calls).toContain('sudo dnf install -y git curl');
    // Enter takes the default: yes.
    expect(existsSync(join(home, '.local', 'bin', 'gh'))).toBe(true);
    expect(r.out).toContain('git, curl, gh, Claude Code');
  });

  it("skips gh on no, saying AgentK asks for it later, and asks nothing else when it's all that's missing", async () => {
    have('git', 'curl');
    const r = await runInTerminal('n', 'Install gh, GitHub');
    expect(r.code).toBe(0);
    expect(r.out).not.toContain('Install them now?');
    expect(existsSync(join(home, '.local', 'bin', 'gh'))).toBe(false);
    expect(r.out).toContain("gh isn't installed: AgentK asks for it when you open a PR");
  });

  it('installs nothing on no', async () => {
    have('gh');
    const r = await runInTerminal('n');
    expect(r.code).toBe(1);
    expect(r.calls).toEqual([]);
    expect(r.out).toMatch(/Install them and run meko extension install agentk again/);
  });

  it('offers to sign gh in to GitHub', async () => {
    have('git', 'curl');
    stub(bin, 'gh', `echo "gh $*" >> "${log}"; [ "$1 $2" = "auth login" ]`);
    const r = await runInTerminal('y', 'Sign in now (gh auth login)?');
    expect(r.code).toBe(0);
    expect(r.calls).toEqual(['gh auth status', 'gh auth login']);
  });

  it('asks which agent to install when none is there, OpenCode second, and installs OpenCode when picked', async () => {
    have('git', 'curl', 'gh');
    rmSync(join(bin, 'claude'));
    stub(repo, 'opencode', `echo "opencode $*" >> "${log}"; [ "$1 $2" = "auth list" ] && echo "Anthropic  Anthropic API key  stored"; exit 0`);
    // OpenCode's installer puts it in ~/.opencode/bin, which isn't on the PATH.
    writeFileSync(join(repo, 'release', 'install'), `mkdir -p "$HOME/.opencode/bin"; cp "${repo}/opencode" "$HOME/.opencode/bin/opencode"; echo "opencode installer ran" >> "${log}"\n`);
    const r = await runInTerminal('2', 'Install Claude Code (1), OpenCode (2) or Kiro CLI (3)?', [['Install them now?', 'y']]);
    expect(r.out).toContain('curl -fsSL https://opencode.ai/v2/install | bash');
    expect(r.code).toBe(0);
    expect(r.calls).toEqual(['curl https://opencode.ai/v2/install', 'opencode installer ran', 'opencode auth list --standalone']);
    expect(r.out).toContain('git, curl, gh, OpenCode');
  });

  it.each([
    ['Claude Code', 'claude', `[ "$1 $2" = "auth status" ] && echo '{ "loggedIn": false }'`, 'claude auth login', ['claude auth login']],
    // OpenCode with no provider signed in (and none's key in the environment).
    ['OpenCode', 'opencode', `[ "$1 $2" = "auth list" ] && echo 'No authenticated integrations'`, 'opencode auth login', ['opencode auth list --standalone', 'opencode auth login --standalone']],
  ])('offers to sign %s in', async (_, cmd, body, login, expected) => {
    have('git', 'curl', 'gh');
    stub(bin, cmd, `echo "${cmd} $*" >> "${log}"; ${body}; exit 0`);
    const r = await runInTerminal('y', `Sign in now (${login})?`);
    expect(r.code).toBe(0);
    expect(r.calls).toEqual(expected);
  });
});

describe('install.sh', () => {
  it("only says Meko installs AgentK now, and fails, for the Mekos and instructions that run it", () => {
    const r = run(['--meko-url', 'http://127.0.0.1:7778/mcp', '--meko-key', 'k'], {}, STUB);
    expect(r.code).toBe(1);
    expect(r.err).toContain('Update Meko first (meko update), then run: meko extension install agentk');
    expect(r.calls).toEqual([]);
    // An older Meko refuses an installer that doesn't mention --meko-url, with a less clear message.
    expect(readFileSync(STUB, 'utf8')).toContain('--meko-url');
  });
});
