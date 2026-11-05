import { readdirSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { kvGet, kvSet, type Db } from './db.js';

/** Set once the Meko call logs AgentK kept itself are gone. */
const DONE_KEY = 'meko-call-logs-removed';

/** The file each session's Meko call log was in, in its folder under `designs`, before Meko kept call contents itself. */
const OLD_LOG_FILE = 'meko-calls.jsonl';

/**
 * Deletes every session's Meko call log AgentK kept before it stopped keeping one,
 * once: they hold what was sent to Meko and what came back, and nothing deletes them any more.
 * Returns how many it deleted. Throws when `designs` can't be read or a log can't be deleted (the
 * first such error, after trying every one); then it isn't marked done and runs again next start.
 */
export function removeOldMekoLogs(db: Db, designs: string): number {
  if (kvGet(db, DONE_KEY) !== null) return 0;
  let removed = 0;
  let folders: string[] = [];
  try {
    folders = readdirSync(designs);
  } catch (e) {
    if ((e as NodeJS.ErrnoException).code !== 'ENOENT') throw e;
  }
  let failed: unknown;
  for (const f of folders) {
    const file = join(designs, f, OLD_LOG_FILE);
    try {
      rmSync(file);
      removed++;
    } catch (e) {
      // Not there, or `f` isn't a folder: nothing to remove. Anything else is tried again next start.
      const code = (e as NodeJS.ErrnoException).code;
      if (code !== 'ENOENT' && code !== 'ENOTDIR') failed ??= e;
    }
  }
  if (failed !== undefined) throw failed;
  kvSet(db, DONE_KEY, '1');
  return removed;
}
