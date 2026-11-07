// Runs before every test file (vitest `setupFiles`). Tests must never touch the developer's
// real state: their ~/.claude.json and ~/.claude, ~/.kiro, OpenCode's config, Meko, GitHub, git config or an installed
// Kode. Scrub the environment a developer's shell (often a Kode session itself) carries,
// and point HOME at an empty folder so login shells, git, gh and Claude settings all start blank.
import { mkdtempSync, realpathSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

for (const k of Object.keys(process.env)) {
  if (/^(MEKO_|KODE_|AK_|CLAUDE|KIRO_|OPENCODE|GH_|GIT_|GITHUB_|TMUX|XDG_CONFIG_HOME$|XDG_STATE_HOME$|XDG_DATA_HOME$)/.test(k)) delete process.env[k];
}
const home = realpathSync(mkdtempSync(join(tmpdir(), 'akl-home-')));
process.env.HOME = home;
process.env.GIT_CONFIG_NOSYSTEM = '1';
