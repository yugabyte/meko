// The command in ~/.local/bin: me_koder, named so a process list, ~/.local/bin and the services say
// AgentK is a Meko extension. It was `agentk` before, and `agent-k-local` until the rename to AgentK;
// a release still carries both in bin/, as links to me_koder, for the services and Mekos that run
// them. Meko's install (`meko extension install agentk`) links `me_koder`, and Meko's start moves an
// install made under an old name onto it, removing the old names' links (extensions.Registry.Rename).
// An update doesn't relink, so the server adds `me_koder` when it starts too (`linkCommand`), for a
// Meko that doesn't yet; it never removes an old link, which that Meko's registry may still run.
import { existsSync, lstatSync, readlinkSync, renameSync, rmSync, symlinkSync } from 'node:fs';
import { homedir } from 'node:os';
import { join } from 'node:path';
import type { Paths } from './paths.js';

export const COMMAND = 'me_koder';
/** The command's names before, newest first. */
export const OLD_COMMANDS = ['agentk', 'agent-k-local'] as const;

/** The command's links in ~/.local/bin, every name. */
export function commandLinks(home: string = homedir()): string[] {
  return [COMMAND, ...OLD_COMMANDS].map((c) => join(home, '.local', 'bin', c));
}

/**
 * Links ~/.local/bin/me_koder to this home's release when a link of an old name there is the one
 * to it and nothing is called me_koder there yet. Returns whether it made the link.
 */
export function linkCommand(paths: Paths, home: string = homedir()): boolean {
  const [link, ...olds] = commandLinks(home);
  const bin = join(paths.app, 'current', 'bin');
  const ours = OLD_COMMANDS.some((c, i) => {
    try {
      return readlinkSync(olds[i]) === join(bin, c);
    } catch {
      return false;
    }
  });
  if (!ours || !existsSync(join(bin, COMMAND))) return false;
  try {
    lstatSync(link);
    return false;
  } catch {
    // free
  }
  symlinkSync(join(bin, COMMAND), link);
  return true;
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
