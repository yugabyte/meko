// A release on probation. Meko installs Presenter's releases in <home>/app/<version> and updates
// them (meko update_extension): as it switches app/current to a new version it writes
// app/update-pending.json, {"from", "to", "attempts", ...}. The launcher (packaging/meko_presenter)
// counts each start of the new version and rolls back to the old one after three that failed; the
// server clears the record once it has served for HEALTHY_MS, and a clean stop takes its start back
// off the count, so only crashes count. These are Meko's own rules for itself (its Healthy and
// Uncount), on the same file. A build from source (no PRESENTER_APP_DIR) has no record.

import { readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import { basename, dirname, join } from 'node:path';

/** How long a new version must serve before its update counts as done. */
export const HEALTHY_MS = 5 * 60_000;

interface Pending {
  from: string;
  to: string;
  attempts?: number;
}

/** The record beside the release folder appDir (app/<version>, which the launcher exports). */
export function pendingFile(appDir: string): string {
  return join(dirname(appDir), 'update-pending.json');
}

function read(file: string): Pending | null {
  try {
    const p = JSON.parse(readFileSync(file, 'utf8')) as Pending;
    return typeof p.to === 'string' ? p : null;
  } catch {
    return null;
  }
}

/** Finishes the update to the running version: its record goes. A record of another version stays. */
export function markHealthy(appDir: string | undefined = process.env.PRESENTER_APP_DIR): void {
  if (!appDir) return;
  const file = pendingFile(appDir);
  if (read(file)?.to === basename(appDir)) rmSync(file, { force: true });
}

/** Takes a clean stop of the version on probation off its count of starts. */
export function uncountStart(appDir: string | undefined = process.env.PRESENTER_APP_DIR): void {
  if (!appDir) return;
  const file = pendingFile(appDir);
  const p = read(file);
  if (!p || p.to !== basename(appDir) || !p.attempts) return;
  // The other fields (Meko's schema and at) are kept as they were.
  const raw = JSON.parse(readFileSync(file, 'utf8')) as Record<string, unknown>;
  const tmp = `${file}.tmp`;
  writeFileSync(tmp, JSON.stringify({ ...raw, attempts: p.attempts - 1 }), { mode: 0o600 });
  renameSync(tmp, file);
}
