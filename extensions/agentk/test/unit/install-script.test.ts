import { spawnSync, execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { chmodSync, existsSync, mkdirSync, readFileSync, readlinkSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import pty from 'node-pty';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { tempDir } from '../helpers.js';

// Runs the real scripts/install.sh (mostly up to --deps-only) against stand-in tools: a PATH with only
// the basic utilities it needs, plus stubs for uname, package managers, sudo, curl, gh and friends.
const SCRIPT = fileURLToPath(new URL('../../scripts/install.sh', import.meta.url));
const BUNDLE = fileURLToPath(new URL('../../scripts/offline-bundle.sh', import.meta.url));
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
case $url in
  # AgentK's releases (see release() below): the listing, and the assets of its one tag.
  https://api.github.com/repos/*/releases?per_page=100) cat "${repo}/releases.json" ;;
  https://github.com/yugabyte/agent-k-local/releases/download/*)
    t=\${url%/*}; [ "\${t##*/}" = "$(cat "${repo}/release-ak.tag")" ] || exit 22
    cp "${repo}/release-ak/\${url##*/}" "$out" ;;
  *) cp "${repo}/release/\${url##*/}" "$out" ;;
esac`);

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
/** jq for the gh stub's --jq, when this machine has it. */
const JQ = spawnSync('sh', ['-c', 'command -v jq'], { encoding: 'utf8' }).stdout.trim();

const calls = () =>
  (existsSync(log) ? readFileSync(log, 'utf8').trim().split('\n') : []).filter((c) => c && c !== 'claude auth status --json');

/** Puts a repo tool on PATH as if it were already installed. */
const have = (...tools: string[]) => tools.forEach((t) => execFileSync('cp', [join(repo, t), join(bin, t)]));

function run(args: string[], env: Record<string, string> = {}) {
  return runAll(['--deps-only', ...args], env);
}

/** Runs the whole installer, past the tools to the download and `agentk install`. */
function runAll(args: string[], env: Record<string, string> = {}) {
  const r = spawnSync(join(bin, 'sh'), [SCRIPT, ...args], {
    env: { PATH: bin, HOME: home, LANG: 'C', AK_BREW_DIRS: '', ...env },
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
    expect(r.err).toContain('sudo -n dnf install -y git curl');
    expect(r.err).toContain('download gh from github.com/cli/cli releases into ~/.local/bin');
    expect(r.err).toMatch(/Rerun with --install-deps/);
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
    expect(r.err).toContain('installing git curl failed (see the output above). Install them yourself and run this installer again.');
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

  it('goes on without gh when GITHUB_TOKEN can do the downloads, unless told to install it', () => {
    have('git', 'curl');
    const r = run(['--non-interactive'], { GITHUB_TOKEN: 't' });
    expect(r.code).toBe(0);
    expect(r.err).toMatch(/gh isn't installed: downloading with GITHUB_TOKEN/);
    const installed = run(['--non-interactive', '--install-deps'], { GITHUB_TOKEN: 't' });
    expect(installed.code).toBe(0);
    expect(existsSync(join(home, '.local', 'bin', 'gh'))).toBe(true);
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
      const wrap = 'sh "$0" --deps-only; s=$?; printf "\\n[exited %s]" "$s"; read -r _; exit "$s"';
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

  it('asks before installing anything, and installs on yes', async () => {
    have('gh');
    const r = await runInTerminal('y');
    expect(r.out).toContain('sudo dnf install -y git curl');
    expect(r.code).toBe(0);
    expect(r.calls).toContain('sudo dnf install -y git curl');
  });

  it('installs nothing on no', async () => {
    have('gh');
    const r = await runInTerminal('n');
    expect(r.code).toBe(1);
    expect(r.calls).toEqual([]);
    expect(r.out).toMatch(/Install them and run this installer again/);
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
    stub(repo, 'opencode', `echo "opencode $*" >> "${log}"; [ "$1 $2" = "auth list" ] && echo "└  1 credentials"; exit 0`);
    // OpenCode's installer puts it in ~/.opencode/bin, which isn't on the PATH.
    writeFileSync(join(repo, 'release', 'install'), `mkdir -p "$HOME/.opencode/bin"; cp "${repo}/opencode" "$HOME/.opencode/bin/opencode"; echo "opencode installer ran" >> "${log}"\n`);
    const r = await runInTerminal('2', 'Install Claude Code (1), OpenCode (2) or Kiro CLI (3)?', [['Install them now?', 'y']]);
    expect(r.out).toContain('curl -fsSL https://opencode.ai/install | bash');
    expect(r.code).toBe(0);
    expect(r.calls).toEqual(['curl https://opencode.ai/install', 'opencode installer ran', 'opencode auth list']);
    expect(r.out).toContain('git, curl, gh, OpenCode');
  });

  it.each([
    ['Claude Code', 'claude', `[ "$1 $2" = "auth status" ] && echo '{ "loggedIn": false }'`, 'claude auth login', ['claude auth login']],
    // OpenCode with no provider signed in (and none's key in the environment).
    ['OpenCode', 'opencode', `[ "$1 $2" = "auth list" ] && printf '┌  Credentials\\n└  0 credentials\\n'`, 'opencode auth login', ['opencode auth list', 'opencode auth login']],
  ])('offers to sign %s in', async (_, cmd, body, login, expected) => {
    have('git', 'curl', 'gh');
    stub(bin, cmd, `echo "${cmd} $*" >> "${log}"; ${body}; exit 0`);
    const r = await runInTerminal('y', `Sign in now (${login})?`);
    expect(r.code).toBe(0);
    expect(r.calls).toEqual(expected);
  });
});

describe('install.sh: when AgentK is already installed', () => {
  const akHome = () => join(home, '.agent-k-local');
  const current = (h = akHome()) => readlinkSync(join(h, 'app', 'current'));

  /**
   * An install of `version`: unpacked, linked as app/current and as ~/.local/bin/agentk and
   * agent-k-local, and set up (config.toml, and both services, as long as there's no `no-services`
   * file). `old`: one from before the rename to AgentK, with only agent-k-local; `oldServices`: its
   * services still under their old names.
   */
  function installed(version: string, h = akHome(), { old = false, oldServices = old }: { old?: boolean; oldServices?: boolean } = {}) {
    mkdirSync(join(h, 'app', version, 'bin'), { recursive: true });
    stub(join(h, 'app', version, 'bin'), old ? 'agent-k-local' : 'agentk', 'exit 0');
    symlinkSync(version, join(h, 'app', 'current'));
    writeFileSync(join(h, 'config.toml'), '');
    mkdirSync(join(home, '.local', 'bin'), { recursive: true });
    for (const c of old ? ['agent-k-local'] : ['agentk', 'agent-k-local']) {
      symlinkSync(join(h, 'app', 'current', 'bin', c), join(home, '.local', 'bin', c));
    }
    const services = `[ -f "${root}/no-services" ] && exit 1`;
    const [unit, label] = oldServices ? ['agent-k-local', 'com.yugabyte.agent-k-local'] : ['agentk', 'com.yugabyte.agentk'];
    stub(bin, 'systemctl', `[ "$*" = "--user is-enabled ${unit}.service" ] || [ "$*" = "--user is-enabled ${unit}-host.service" ] || exit 1; ${services}; echo enabled`);
    stub(bin, 'launchctl', `case $2 in gui/1000/${label}|gui/1000/${label}.host) ;; *) exit 1 ;; esac; ${services}; exit 0`);
  }

  /**
   * Release `version` of AgentK under `tag`, as `gh release download` (or curl) hands it out,
   * listed after a newer Meko release, a prerelease and a draft (the repo's latest is Meko's).
   * Its tarball is agentk-<version>-…, or with `old` agent-k-local-<version>-…, the only one a
   * release from before the rename to AgentK has.
   */
  function release(version: string, tag = `v${version}`, { old = false } = {}) {
    const name = old ? 'agent-k-local' : 'agentk';
    const dir = join(repo, 'ak', `${name}-${version}`);
    mkdirSync(join(dir, 'bin'), { recursive: true });
    mkdirSync(join(dir, 'node', 'bin'), { recursive: true });
    stub(join(dir, 'bin'), name, `echo "${name} $*" >> "${log}"`);
    if (!old) symlinkSync('agentk', join(dir, 'bin', 'agent-k-local'));
    // Stands in for the bundled Node renaming app/current.tmp over app/current.
    stub(join(dir, 'node', 'bin'), 'node', 'ln -sfn "$(readlink "$3")" "$4" && rm "$3"');
    const tarball = `${name}-${version}-linux-x64.tar.gz`;
    rmSync(join(repo, 'release-ak'), { recursive: true, force: true });
    mkdirSync(join(repo, 'release-ak'), { recursive: true });
    execFileSync('tar', ['-czf', join(repo, 'release-ak', tarball), `${name}-${version}`], { cwd: join(repo, 'ak') });
    const digest = createHash('sha256').update(readFileSync(join(repo, 'release-ak', tarball))).digest('hex');
    writeFileSync(join(repo, 'release-ak', 'SHA256SUMS'), `${digest}  ${tarball}\n`);
    writeFileSync(join(repo, 'release-ak.tag'), tag);
    const listed = [
      { tag_name: 'meko-v9.9.9', draft: false, prerelease: false },
      { tag_name: 'agentk-v9.9.8', draft: true, prerelease: false },
      { tag_name: 'agentk-v9.9.7-beta.1', draft: false, prerelease: true },
      { tag_name: tag, draft: false, prerelease: false },
    ];
    // Pretty, one key per line, as GitHub's API answers curl.
    writeFileSync(join(repo, 'releases.json'), JSON.stringify(listed, null, 2));
    // gh's --jq is built in: here it's the real jq when there is one.
    const api = JQ ? `while [ $# -gt 0 ]; do [ "$1" = --jq ] && q=$2; shift; done; "${JQ}" -r "$q" "${repo}/releases.json"` : `echo "${tag}"`;
    stub(bin, 'gh', `echo "gh $*" >> "${log}"
case "$1 $2" in
  "api repos/yugabyte/agent-k-local/releases?per_page=100") ${api} ;;
  "release download") [ "$3" = "${tag}" ] || { echo "release not found" >&2; exit 1; }
    while [ $# -gt 0 ]; do [ "$1" = --dir ] && cp "${repo}/release-ak/"* "$2/"; shift; done ;;
esac
exit 0`);
  }

  const downloads = (calls: string[]) => calls.filter((c) => /^gh (release|api) /.test(c));

  beforeEach(() => {
    have('git', 'curl');
    installed('1.2.3');
    release('1.2.4');
  });

  it('says so and stops, changing nothing', () => {
    const r = runAll([]);
    expect(r.code).toBe(0);
    expect(r.err).toContain('1.2.3 in ~/.agent-k-local');
    expect(r.err).toContain('AgentK is already installed, so nothing was changed');
    expect(r.err).toContain('Update banner or button');
    // ~/.local/bin isn't on this PATH, so the commands are shown with it.
    expect(r.err).toContain('~/.local/bin/agentk status');
    expect(r.err).toContain('--output - | sh -s -- --reinstall');
    expect(r.calls).toEqual([]);
    expect(current()).toBe('1.2.3');

    const onPath = runAll([], { PATH: `${bin}:${join(home, '.local', 'bin')}` });
    expect(onPath.code).toBe(0);
    expect(onPath.err).toMatch(/Status\S* +agentk status/);
  });

  it('counts services still under their names from before the rename to AgentK as set up', () => {
    rmSync(join(akHome(), 'app'), { recursive: true });
    rmSync(join(home, '.local', 'bin'), { recursive: true });
    installed('1.2.3', akHome(), { oldServices: true });
    const r = runAll([]);
    expect(r.code).toBe(0);
    expect(r.err).toContain('AgentK is already installed');
  });

  it('installs an install from before the rename to AgentK again, which adds the agentk command', () => {
    rmSync(join(akHome(), 'app'), { recursive: true });
    rmSync(join(home, '.local', 'bin'), { recursive: true });
    installed('1.2.3', akHome(), { old: true });
    const r = runAll(['--non-interactive']);
    expect(r.err).not.toContain('already installed');
    expect(r.code).toBe(0);
    expect(r.calls).toContain('agentk install --non-interactive');
    for (const c of ['agentk', 'agent-k-local']) expect(readlinkSync(join(home, '.local', 'bin', c))).toBe(join(akHome(), 'app', 'current', 'bin', c));
  });

  it("installs a release from before the rename to AgentK by its tarball's old name", () => {
    release('1.2.0', 'v1.2.0', { old: true });
    const r = runAll(['--version', '1.2.0', '--non-interactive']);
    expect(r.code, r.err).toBe(0);
    expect(current()).toBe('1.2.0');
    expect(r.calls).toContain('agent-k-local install --non-interactive');
  });

  it('stops before the tools, even with tools missing and --install-deps', () => {
    rmSync(join(bin, 'git'));
    const r = runAll(['--non-interactive', '--install-deps']);
    expect(r.code).toBe(0);
    expect(r.err).toContain('AgentK is already installed');
    expect(r.err).not.toContain('Missing');
    expect(r.calls).toEqual([]);
  });

  it('installs the latest release again with --reinstall, without passing that on', () => {
    const r = runAll(['--reinstall', '--non-interactive']);
    expect(r.err).not.toContain('already installed');
    expect(r.code).toBe(0);
    expect(downloads(r.calls)).toHaveLength(2);
    expect(r.calls).toContain('agentk install --non-interactive');
    expect(current()).toBe('1.2.4');
  });

  it('installs another --version, and stops for the one installed', () => {
    const same = runAll(['--version', 'v1.2.3']);
    expect(same.code).toBe(0);
    expect(same.err).toContain('AgentK is already installed');
    expect(same.calls).toEqual([]);

    // Either tag scheme's: agentk-v first, then v; each with the tarball's name, then its old one.
    const other = runAll(['--version', '1.2.4']);
    expect(other.code).toBe(0);
    expect(downloads(other.calls)).toEqual([
      expect.stringMatching(/^gh release download agentk-v1\.2\.4 .*--pattern agentk-1\.2\.4-/),
      expect.stringMatching(/^gh release download agentk-v1\.2\.4 .*--pattern agent-k-local-1\.2\.4-/),
      expect.stringMatching(/^gh release download v1\.2\.4 .*--pattern agentk-1\.2\.4-/),
    ]);
    expect(current()).toBe('1.2.4');
  });

  it.each([
    ['gh', {}],
    ['curl and GITHUB_TOKEN', { GITHUB_TOKEN: 't' }],
  ])('installs the newest AgentK release, not the repo\'s latest (Meko\'s), with %s', (_name, env) => {
    release('1.3.0', 'agentk-v1.3.0');
    if ('GITHUB_TOKEN' in env) rmSync(join(bin, 'gh'));
    const r = runAll(['--reinstall', '--non-interactive'], env);
    expect(r.code, r.err).toBe(0);
    expect(current()).toBe('1.3.0');
    if (!('GITHUB_TOKEN' in env)) expect(downloads(r.calls)[1]).toMatch(/^gh release download agentk-v1\.3\.0 /);
  });

  it.each([
    // An install that failed at its services, or was uninstalled.
    ["the services aren't set up", () => writeFileSync(join(root, 'no-services'), '')],
    ['~/.local/bin/agentk is gone', () => rmSync(join(home, '.local', 'bin', 'agentk'))],
    // An install that stopped before its setup was saved.
    ['config.toml is missing', () => rmSync(join(akHome(), 'config.toml'))],
  ])('installs again when %s', (_, breakIt) => {
    breakIt();
    const r = runAll(['--non-interactive']);
    expect(r.err).not.toContain('already installed');
    expect(r.code).toBe(0);
    expect(r.calls).toContain('agentk install --non-interactive');
    expect(current()).toBe('1.2.4');
    expect(existsSync(join(home, '.local', 'bin', 'agentk'))).toBe(true);
  });

  it('passes options for agentk install on rather than dropping them', () => {
    const r = runAll(['--non-interactive', '--port', '8443', '--no-meko']);
    expect(r.err).not.toContain('already installed');
    expect(r.code).toBe(0);
    expect(r.calls).toContain("agentk install --non-interactive --port 8443 --no-meko");
  });

  it('checks the LaunchAgents on a Mac', () => {
    const env = { FAKE_OS: 'Darwin', FAKE_ARCH: 'arm64' };
    const set = runAll([], env);
    expect(set.code).toBe(0);
    expect(set.err).toContain('AgentK is already installed');
    expect(set.calls).toEqual([]);

    writeFileSync(join(root, 'no-services'), '');
    const unset = runAll(['--non-interactive'], env);
    expect(unset.err).not.toContain('already installed');
    expect(downloads(unset.calls).length).toBeGreaterThan(0);
  });

  it("installs into Meko's data folder (MEKO_HOME), whatever AK_HOME says, once nothing is in the old home", () => {
    rmSync(akHome(), { recursive: true });
    const meko = join(root, 'meko');
    const r = runAll(['--non-interactive'], { MEKO_HOME: meko, AK_HOME: join(root, 'other-home') });
    expect(r.code).toBe(0);
    expect(r.calls).toContain('agentk install --non-interactive');
    expect(current(join(meko, 'extensions', 'agentk'))).toBe('1.2.4');
    expect(existsSync(join(root, 'other-home'))).toBe(false);
  });

  describe('from a local tarball (--tarball)', () => {
    const local = (v = '1.2.4') => join(repo, 'release-ak', `agentk-${v}-linux-x64.tar.gz`);

    it('installs it, checked against the SHA256SUMS beside it, without downloading anything', () => {
      const r = runAll(['--tarball', local(), '--non-interactive']);
      expect(r.code, r.err).toBe(0);
      expect(r.err).toContain('1.2.4 (linux-x64), checksum verified');
      expect(downloads(r.calls)).toEqual([]);
      expect(r.calls.filter((c) => c.startsWith('curl'))).toEqual([]);
      expect(r.calls).toContain('agentk install --non-interactive');
      expect(current()).toBe('1.2.4');
    });

    it.each([
      ['a checksum that doesn\'t match', () => writeFileSync(join(repo, 'release-ak', 'SHA256SUMS'), `${'0'.repeat(64)}  agentk-1.2.4-linux-x64.tar.gz\n`), 'checksum verification failed'],
      ['no SHA256SUMS', () => rmSync(join(repo, 'release-ak', 'SHA256SUMS')), 'no SHA256SUMS beside'],
    ])('refuses one with %s', (_, breakIt, message) => {
      breakIt();
      const r = runAll(['--tarball', local(), '--non-interactive']);
      expect(r.code).toBe(1);
      expect(r.err).toContain(message);
      expect(current()).toBe('1.2.3');
    });

    const mac = { FAKE_OS: 'Darwin', FAKE_ARCH: 'arm64' };
    it.each([
      ['a tarball for another computer', () => [local()], mac, "isn't for this computer (darwin-arm64)"],
      ['a folder with none for this computer', () => [join(repo, 'release-ak')], mac, 'no AgentK tarball for this computer (darwin-arm64)'],
      ['a folder with two for this computer', () => {
        execFileSync('cp', [local(), local('1.2.5')]);
        return [join(repo, 'release-ak')];
      }, {}, 'more than one AgentK tarball for this computer'],
      ['--version beside it', () => [local(), '--version', '1.2.4'], {}, 'leave out --version'],
    ])('refuses %s', (_, args, env, message) => {
      const r = runAll(['--tarball', ...args(), '--non-interactive'], env);
      expect(r.code).toBe(1);
      expect(r.err).toContain(message);
      expect(current()).toBe('1.2.3');
    });

    it('stops for the version installed, offering a reinstall from the same tarball', () => {
      release('1.2.3');
      const r = runAll(['--tarball', join(repo, 'release-ak')]);
      expect(r.code).toBe(0);
      expect(r.err).toContain('AgentK is already installed');
      expect(r.err).toContain(`--tarball ${join(repo, 'release-ak')} --reinstall`);
      expect(r.err).not.toContain('gh release download');
      expect(r.calls).toEqual([]);
    });

    it('installs from an offline bundle made by offline-bundle.sh', () => {
      release('1.2.6', 'agentk-v1.2.6');
      execFileSync('cp', [SCRIPT, join(repo, 'release-ak', 'install.sh')]);
      const out = join(root, 'out');
      const made = spawnSync(join(bin, 'sh'), [BUNDLE, 'v1.2.6', out], { env: { PATH: bin, HOME: home }, encoding: 'utf8' });
      expect(made.status, made.stderr).toBe(0);
      expect(made.stdout.trim()).toBe(join(out, 'agentk-1.2.6-offline.tar.gz'));
      execFileSync('tar', ['-xzf', 'agentk-1.2.6-offline.tar.gz'], { cwd: out });
      const dir = join(out, 'agentk-1.2.6-offline');
      for (const f of ['install.sh', 'install-offline.sh', 'agentk-1.2.6-linux-x64.tar.gz', 'SHA256SUMS', 'LICENSE']) expect(existsSync(join(dir, f)), f).toBe(true);

      writeFileSync(log, '');
      const r = spawnSync(join(bin, 'sh'), [join(dir, 'install-offline.sh'), '--non-interactive'], {
        env: { PATH: bin, HOME: home, LANG: 'C', AK_BREW_DIRS: '' },
        encoding: 'utf8',
      });
      expect(r.status, r.stderr).toBe(0);
      expect(downloads(calls())).toEqual([]);
      expect(current()).toBe('1.2.6');
    });

    it("won't bundle a release whose installer can't install from a tarball", () => {
      release('1.2.6', 'agentk-v1.2.6');
      writeFileSync(join(repo, 'release-ak', 'install.sh'), '#!/bin/sh\n');
      const made = spawnSync(join(bin, 'sh'), [BUNDLE, '1.2.6', join(root, 'out')], { env: { PATH: bin, HOME: home }, encoding: 'utf8' });
      expect(made.status).toBe(1);
      expect(made.stderr).toContain("can't install from a tarball");
    });
  });

  it('still checks the tools with --deps-only', () => {
    const r = run(['--non-interactive']);
    expect(r.code).toBe(0);
    expect(r.err).toContain('Tools');
    expect(r.err).not.toContain('already installed');
  });
});

describe('install.sh: where AgentK lives', () => {
  /** Where Meko's data folder is: the cases Meko's paths.Resolve and AgentK's mekoHome are tested against too. */
  const CASES = (JSON.parse(readFileSync(join(__dirname, '../../../../backend/internal/paths/testdata/resolve-cases.json'), 'utf8')) as {
    cases: { name: string; env: Record<string, string>; networkDirs: string[]; want?: string; error?: boolean }[];
  }).cases;

  // $SHARED_TMP is /var/tmp itself here: --print-home makes nothing. Other absolute paths go under
  // the test's folder, the network folders made, since the check looks at the nearest folder there.
  it.each(CASES)('$name', (c) => {
    const fill = (p: string) => {
      const v = p.replace('$HOME', home).replace('$SHARED_TMP', '/var/tmp').replace('$USER', 'me');
      return v.startsWith(home) || v.startsWith('/var/tmp') || !v.startsWith('/') ? v : join(root, v);
    };
    const network = c.networkDirs.map(fill);
    for (const d of network) mkdirSync(d, { recursive: true });
    stub(bin, 'stat', `for d in ${network.map((d) => `'${d}'`).join(' ')}; do case $4 in "$d"|"$d"/*) echo nfs; exit 0 ;; esac; done; echo ext2/ext3`);
    const env = Object.fromEntries(Object.entries(c.env).map(([k, v]) => [k, fill(v)]));
    const r = spawnSync(join(bin, 'sh'), [SCRIPT, '--print-home'], { env: { PATH: bin, HOME: home, LANG: 'C', FAKE_UID: 'me', ...env }, encoding: 'utf8' });
    if (c.error) {
      expect(r.status).toBe(1);
      expect(r.stderr).toContain('network filesystem');
    } else {
      expect(r.stdout.trim()).toBe(join(fill(c.want!), 'extensions', 'agentk'));
    }
  });

  it("finds an install made with a custom AK_HOME by the command link it made, so a rerun updates it there", () => {
    const custom = join(root, 'custom-home');
    mkdirSync(join(custom, 'app'), { recursive: true });
    mkdirSync(join(home, '.local', 'bin'), { recursive: true });
    symlinkSync(join(custom, 'app', 'current', 'bin', 'agentk'), join(home, '.local', 'bin', 'agentk'));
    const r = spawnSync(join(bin, 'sh'), [SCRIPT, '--print-home'], { env: { PATH: bin, HOME: home, LANG: 'C' }, encoding: 'utf8' });
    expect(r.stdout.trim()).toBe(custom);
  });

  it("keeps an install from before the move in its old home until its server moves it", () => {
    mkdirSync(join(home, '.agent-k-local'));
    const r = spawnSync(join(bin, 'sh'), [SCRIPT, '--print-home'], { env: { PATH: bin, HOME: home, LANG: 'C' }, encoding: 'utf8' });
    expect(r.stdout.trim()).toBe(join(home, '.agent-k-local'));
  });
});
