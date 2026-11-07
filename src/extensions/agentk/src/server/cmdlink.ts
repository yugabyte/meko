// Kode's program: me_kode, named so a process list and the services say Kode is a Meko
// extension. It isn't a command on PATH: people run it through Meko (`meko kode <command>`), which
// runs a release's app/current/bin/me_kode. Its names before were `me_koder`, `agentk` and, until
// the rename to Kode, `agent-k-local`: a release still carries the first two in bin/, as links to
// me_kode, for the services and Mekos that run them. Installs once linked them into ~/.local/bin;
// Meko's start removes those links (extensions.Registry.Rename), and Kode never makes one.
import { readlinkSync, renameSync, rmSync, symlinkSync } from 'node:fs';
import { homedir } from 'node:os';
import { join } from 'node:path';

export const COMMAND = 'me_kode';
/** The command's names before, newest first. */
export const OLD_COMMANDS = ['me_koder', 'agentk', 'agent-k-local'] as const;

/** The ~/.local/bin links an older install may have left, every name: `uninstall` removes them, and the home's move repoints them. */
export function commandLinks(home: string = homedir()): string[] {
  return [COMMAND, ...OLD_COMMANDS].map((c) => join(home, '.local', 'bin', c));
}

/**
 * Points the command's links into a moved home at their place in its new one (movehome.ts).
 * Links that point elsewhere, or aren't there, are left alone. Returns the links it changed.
 */
export function repointLinks(map: (p: string) => string | null, home: string = homedir()): string[] {
  const moved: string[] = [];
  for (const link of commandLinks(home)) {
    let target: string;
    try {
      target = readlinkSync(link);
    } catch {
      continue;
    }
    const to = map(target);
    if (!to) continue;
    // A new link under a temp name, renamed over the old one: the command is never missing.
    rmSync(`${link}.moving`, { force: true }); // left by a run cut off here
    symlinkSync(to, `${link}.moving`);
    renameSync(`${link}.moving`, link);
    moved.push(link);
  }
  return moved;
}
