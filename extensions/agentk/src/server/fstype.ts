// Filesystem types (to keep SQLite and AgentK's home off network filesystems) and private folders
// in shared places like /var/tmp. Meko's backend/internal/paths checks the same names.
import { execFileSync } from 'node:child_process';
import { chmodSync, lstatSync, mkdirSync } from 'node:fs';
import { dirname } from 'node:path';

// Linux names from `stat -f -c %T`; macOS names from `mount`.
const NETWORK_FS = new Set(['nfs', 'nfs4', 'cifs', 'smb2', 'smbfs', 'fuse.sshfs', 'afs', 'lustre', 'gpfs', 'afpfs', 'webdav', 'macfuse', 'osxfuse']);

/** A private folder that can't be made safely; the message says how to fix it. */
export class PrivateDirError extends Error {}

function trySh(cmd: string, args: string[]): string | null {
  try {
    return execFileSync(cmd, args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim();
  } catch {
    return null;
  }
}

/** The mount point in `df -P <dir>` output (the text after the capacity column). */
export function parseDfMountPoint(df: string): string | null {
  const last = df.trim().split('\n').at(-1) ?? '';
  return /\s\d+%\s+(.+)$/.exec(last)?.[1] ?? null;
}

/** The filesystem type of `mountPoint` in macOS `mount` output ("<dev> on <dir> (<type>, …)"). */
export function parseMountType(mount: string, mountPoint: string): string | null {
  const marker = ` on ${mountPoint} (`;
  for (const line of mount.split('\n')) {
    const i = line.lastIndexOf(marker);
    if (i >= 0) return /^[^,)]+/.exec(line.slice(i + marker.length))?.[0].trim() ?? null;
  }
  return null;
}

export function fsType(dir: string): string | null {
  if (process.platform !== 'darwin') return trySh('stat', ['-f', '-c', '%T', dir]);
  // BSD stat has no -c: find the mount with df, then read its type from mount.
  const df = trySh('df', ['-P', dir]);
  const mp = df ? parseDfMountPoint(df) : null;
  const mount = mp ? trySh('mount', []) : null;
  return mp && mount ? parseMountType(mount, mp) : null;
}

/** Whether `dir` is on a network filesystem; false when that can't be told. */
export function isNetworkFs(dir: string, type: (dir: string) => string | null = fsType): boolean {
  const t = type(dir);
  return !!t && NETWORK_FS.has(t);
}

/**
 * Creates `dir` for private state in a shared place like /var/tmp, where another user could have
 * made the directories first. Refuses unless `dir` and its parent are real directories (not
 * symlinks) owned by this user and the parent isn't writable by others; then makes `dir` 0700.
 */
export function ensurePrivateDir(
  dir: string,
  uid: number | undefined = process.getuid?.(),
  gid: number | undefined = process.getgid?.(),
  fix = 'Remove it, or set state.db_path in config.toml to a local folder only you can write to.',
): void {
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  for (const d of [dirname(dir), dir]) {
    const st = lstatSync(d);
    if (st.isSymbolicLink() || !st.isDirectory()) throw new PrivateDirError(`${d} is not a directory (it may be a symlink). ${fix}`);
    if (uid !== undefined && st.uid !== uid) throw new PrivateDirError(`${d} is owned by another user. ${fix}`);
    // Group write is fine for our own primary group (a umask 002 system with a group per user).
    const othersWrite = st.mode & 0o002 || (st.mode & 0o020 && (gid === undefined || st.gid !== gid));
    if (d !== dir && othersWrite) throw new PrivateDirError(`${d} can be written to by other users. ${fix}`);
  }
  chmodSync(dir, 0o700);
}
