// Runs before every test file. Tests must never touch the developer's real state: their Meko, their
// Claude Code, or an installed Present. Scrub what a developer's shell carries, and point HOME at
// an empty folder.
import { mkdtempSync, realpathSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

for (const k of Object.keys(process.env)) {
  if (/^(MEKO_|PRESENT_|CLAUDE|ANTHROPIC_|XDG_STATE_HOME$|XDG_CONFIG_HOME$)/.test(k)) delete process.env[k];
}
process.env.HOME = realpathSync(mkdtempSync(join(tmpdir(), 'pres-home-')));
