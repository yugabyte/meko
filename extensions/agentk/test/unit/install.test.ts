import { chmodSync, existsSync, mkdirSync, mkdtempSync, readdirSync, rmSync, statSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { accessBind, accessDefault, ensurePrivateDir, isHeadless, parseDfMountPoint, parseMountType, purgeHome, removeDb, serviceFailure, sessionWorktrees } from '../../src/server/install.js';
import { pad } from '../../src/server/tty.js';
import { getPaths } from '../../src/server/paths.js';
import { LaunchdServices, SystemdServices } from '../../src/server/service.js';

// Output of `df -P` and `mount` on macOS, used to find the filesystem type there.
const DF = `Filesystem     512-blocks      Used Available Capacity  Mounted on
/dev/disk3s5    965595304 612345678 331234567    65%    /System/Volumes/Data
`;

const DF_SPACES = `Filesystem                  512-blocks Used Available Capacity Mounted on
//me@nas.local/Team%20Share  1000000   500   999500     1%   /Volumes/Team Share
`;

const MOUNT = `/dev/disk3s1s1 on / (apfs, sealed, local, read-only, journaled)
devfs on /dev (devfs, local, nobrowse)
/dev/disk3s5 on /System/Volumes/Data (apfs, local, journaled, nobrowse, protect, root data)
map auto_home on /System/Volumes/Data/home (autofs, automounted, nobrowse)
//me@nas.local/Team%20Share on /Volumes/Team Share (smbfs, nodev, nosuid, mounted by me)
nas.local:/export/home on /Volumes/home (nfs, asynchronous)
`;

describe('macOS filesystem type', () => {
  it('reads the mount point from df -P', () => {
    expect(parseDfMountPoint(DF)).toBe('/System/Volumes/Data');
    expect(parseDfMountPoint(DF_SPACES)).toBe('/Volumes/Team Share');
    expect(parseDfMountPoint('')).toBeNull();
  });

  it('reads the type of that mount from mount', () => {
    expect(parseMountType(MOUNT, '/System/Volumes/Data')).toBe('apfs');
    expect(parseMountType(MOUNT, '/')).toBe('apfs');
    expect(parseMountType(MOUNT, '/Volumes/Team Share')).toBe('smbfs');
    expect(parseMountType(MOUNT, '/Volumes/missing')).toBeNull();
  });
});

describe('service failures', () => {
  const ok = () => ({ ok: true, out: '', err: '' });

  it('say what to check and how to skip the services', () => {
    const mac = serviceFailure(new LaunchdServices(ok, '/Users/me', 501), 'launchctl bootstrap … failed: Bootstrap failed: 5', '/Users/me/.agent-k-local/logs');
    expect(mac).toContain('launchctl bootstrap … failed: Bootstrap failed: 5\nCheck: launchctl print gui/501/com.yugabyte.agentk and ');
    expect(mac).toMatch(/logs\/launchd\*\.log\./);
    expect(mac).toContain('--no-service');

    const linux = serviceFailure(new SystemdServices(ok, {}, '/home/me'), 'systemctl --user restart agentk.service failed', '/home/me/.agent-k-local/logs');
    expect(linux).toContain('Check: systemctl --user status agentk.service.\n');
    expect(linux).not.toContain('launchd');
  });
});

describe('private state folder in a shared place', () => {
  let root: string;
  beforeEach(() => {
    root = mkdtempSync(join(tmpdir(), 'ak-priv-'));
  });
  afterEach(() => rmSync(root, { recursive: true, force: true }));

  it('creates the folder and its parent, owner-only', () => {
    const dir = join(root, 'me', 'agent-k-local');
    ensurePrivateDir(dir);
    expect(statSync(dir).mode & 0o777).toBe(0o700);
    // An existing folder of ours is tightened.
    chmodSync(dir, 0o755);
    ensurePrivateDir(dir);
    expect(statSync(dir).mode & 0o777).toBe(0o700);
  });

  it('refuses a symlink another user could point anywhere', () => {
    mkdirSync(join(root, 'elsewhere'));
    mkdirSync(join(root, 'me'), { mode: 0o755 });
    symlinkSync(join(root, 'elsewhere'), join(root, 'me', 'agent-k-local'));
    expect(() => ensurePrivateDir(join(root, 'me', 'agent-k-local'))).toThrow(/not a directory/);
    symlinkSync(join(root, 'elsewhere'), join(root, 'you'));
    expect(() => ensurePrivateDir(join(root, 'you', 'agent-k-local'))).toThrow(/not a directory/);
  });

  it('refuses a parent others can write to', () => {
    mkdirSync(join(root, 'me'));
    chmodSync(join(root, 'me'), 0o777);
    expect(() => ensurePrivateDir(join(root, 'me', 'agent-k-local'))).toThrow(/written to by other users/);
  });

  it('accepts a group-writable parent only when the group is our own', () => {
    mkdirSync(join(root, 'me'));
    chmodSync(join(root, 'me'), 0o775);
    const { uid, gid } = statSync(join(root, 'me'));
    expect(() => ensurePrivateDir(join(root, 'me', 'agent-k-local'), uid, gid)).not.toThrow();
    expect(() => ensurePrivateDir(join(root, 'me', 'agent-k-local'), uid, gid + 1)).toThrow(/written to by other users/);
  });

  it('refuses folders owned by another user', () => {
    const uid = statSync(root).uid;
    expect(() => ensurePrivateDir(join(root, 'me', 'agent-k-local'), uid + 1)).toThrow(/owned by another user/);
  });
});

describe('uninstall --purge', () => {
  let root: string;
  beforeEach(() => {
    root = mkdtempSync(join(tmpdir(), 'ak-purge-'));
  });
  afterEach(() => rmSync(root, { recursive: true, force: true }));

  function home() {
    const paths = getPaths(join(root, 'home'));
    for (const d of [paths.app, paths.logs, join(paths.worktrees, 'p1', 's1'), join(paths.worktrees, 'p1', 's2'), join(paths.designs, 'x')]) {
      mkdirSync(d, { recursive: true });
    }
    writeFileSync(paths.config, '');
    writeFileSync(paths.defaultDb, '');
    writeFileSync(join(paths.worktrees, 'p1', 'stray-file'), '');
    return paths;
  }

  it('lists session worktrees', () => {
    const paths = home();
    expect(sessionWorktrees(paths.worktrees).sort()).toEqual([join(paths.worktrees, 'p1', 's1'), join(paths.worktrees, 'p1', 's2')]);
    expect(sessionWorktrees(join(root, 'missing'))).toEqual([]);
  });

  it('keeps session worktrees and designs unless asked to delete them', () => {
    const paths = home();
    expect(purgeHome(paths, false)).toEqual([paths.worktrees, paths.designs]);
    expect(readdirSync(paths.home).sort()).toEqual(['designs', 'worktrees']);
    expect(existsSync(join(paths.worktrees, 'p1', 's1'))).toBe(true);
  });

  it('removes kept folders that are empty, and then the home', () => {
    const paths = getPaths(join(root, 'home'));
    mkdirSync(paths.worktrees, { recursive: true });
    writeFileSync(paths.config, '');
    expect(purgeHome(paths, false)).toEqual([]);
    expect(existsSync(paths.home)).toBe(false);
  });

  it('deletes everything with --delete-worktrees', () => {
    const paths = home();
    expect(purgeHome(paths, true)).toEqual([]);
    expect(existsSync(paths.home)).toBe(false);
  });

  it('deletes only the database files from a folder it shares with other files', () => {
    const dir = join(root, 'me');
    mkdirSync(dir);
    for (const f of ['agentk.db', 'agentk.db-wal', 'agentk.db-shm', 'state.db.bak-0.1.0', 'notes.txt']) writeFileSync(join(dir, f), '');
    removeDb(join(dir, 'agentk.db'));
    expect(readdirSync(dir)).toEqual(['notes.txt']);
  });

  it('removes the database folder when that leaves it empty', () => {
    const dir = join(root, 'var', 'agent-k-local');
    mkdirSync(dir, { recursive: true });
    for (const f of ['state.db', 'state.db-wal', 'state.db.bak-0.1.0']) writeFileSync(join(dir, f), '');
    removeDb(join(dir, 'state.db'));
    expect(existsSync(dir)).toBe(false);
    expect(existsSync(join(root, 'var'))).toBe(true);
  });
});

describe('server PATH', () => {
  it('adds ~/.local/bin (where the installer puts agentk and gh) once', async () => {
    const { withLocalBin } = await import('../../src/server/paths.js');
    expect(withLocalBin('/usr/bin:/bin', '/home/me')).toBe('/usr/bin:/bin:/home/me/.local/bin');
    expect(withLocalBin('/home/me/.local/bin:/usr/bin', '/home/me')).toBe('/home/me/.local/bin:/usr/bin');
    expect(withLocalBin('', '/home/me')).toBe('/home/me/.local/bin');
  });
});

describe('the Access question', () => {
  it('defaults to remote over SSH or on Linux with no display, and to local at a desktop', () => {
    expect(isHeadless({ SSH_CONNECTION: '10.0.0.2 5000 10.0.0.1 22' }, 'darwin')).toBe(true);
    expect(isHeadless({ SSH_TTY: '/dev/pts/1', DISPLAY: ':0' }, 'linux')).toBe(true);
    expect(isHeadless({}, 'linux')).toBe(true);
    expect(isHeadless({ DISPLAY: ':0' }, 'linux')).toBe(false);
    expect(isHeadless({ WAYLAND_DISPLAY: 'wayland-0' }, 'linux')).toBe(false);
    expect(isHeadless({}, 'darwin')).toBe(false);
  });

  it('turns answers into listen addresses', () => {
    expect(accessBind(' L ')).toBe('127.0.0.1');
    expect(accessBind('remote')).toBe('0.0.0.0');
    expect(accessBind('10.1.2.3')).toBe('10.1.2.3');
    // The address it listens on now is kept, even a host name.
    expect(accessBind('devbox.lan', 'devbox.lan')).toBe('devbox.lan');
    // Local and remote keep the address when it already is one: IPv6 stays IPv6.
    expect(accessBind('remote', '::')).toBe('::');
    expect(accessBind('remote', '127.0.0.1')).toBe('0.0.0.0');
    expect(accessBind('local', '::1')).toBe('::1');
    expect(accessBind('local', '::')).toBe('127.0.0.1');
    expect(() => accessBind('remtoe')).toThrow(/local or remote/);
    expect(() => accessBind('')).toThrow(/local or remote/);
  });

  it('starts a reinstall from the current listen address', () => {
    expect(accessDefault('127.0.0.1')).toBe('local');
    expect(accessDefault('::1')).toBe('local');
    expect(accessDefault('0.0.0.0')).toBe('remote');
    expect(accessDefault('10.1.2.3')).toBe('10.1.2.3');
  });
});

describe('CLI labels', () => {
  it('always leave a space before the detail, even when the label is long', () => {
    expect(pad('Meko')).toBe('Meko'.padEnd(14));
    expect(pad('com.yugabyte.agentk.host')).toBe('com.yugabyte.agentk.host ');
  });
});
