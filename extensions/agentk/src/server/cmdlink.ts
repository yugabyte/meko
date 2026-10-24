// The command in ~/.local/bin. It was `agent-k-local` until the rename to AgentK; the installer
// links both names since. An update doesn't run the installer, so an install from before the
// rename gets `agentk` from the server when it next starts (`linkCommand`). The old name stays,
// for the scripts, services and Meko registries that run it.
import { existsSync, lstatSync, readlinkSync, renameSync, symlinkSync } from 'node:fs';
import { homedir } from 'node:os';
import { join } from 'node:path';
import type { Paths } from './paths.js';

export const COMMAND = 'agentk';
export const OLD_COMMAND = 'agent-k-local';

/** The command's links in ~/.local/bin, both names. */
export function commandLinks(home: string = homedir()): string[] {
  return [COMMAND, OLD_COMMAND].map((c) => join(home, '.local', 'bin', c));
}

/**
 * Links ~/.local/bin/agentk to this home's release when ~/.local/bin/agent-k-local is the
 * installer's link to it and nothing is called agentk there yet. Returns whether it made the link.
 */
export function linkCommand(paths: Paths, home: string = homedir()): boolean {
  const [link, old] = commandLinks(home);
  const bin = join(paths.app, 'current', 'bin');
  try {
    if (readlinkSync(old) !== join(bin, OLD_COMMAND)) return false;
  } catch {
    return false;
  }
  if (!existsSync(join(bin, COMMAND))) return false;
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
    symlinkSync(to, `${link}.moving`);
    renameSync(`${link}.moving`, link);
    moved.push(link);
  }
  return moved;
}
