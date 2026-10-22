// The command in ~/.local/bin. It was `agent-k-local` until the rename to AgentK; the installer
// links both names since. An update doesn't run the installer, so an install from before the
// rename gets `agentk` from the server when it next starts (`linkCommand`). The old name stays,
// for the scripts, services and Meko registries that run it.
import { existsSync, lstatSync, readlinkSync, symlinkSync } from 'node:fs';
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
