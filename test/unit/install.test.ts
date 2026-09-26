import { describe, expect, it } from 'vitest';
import { parseDfMountPoint, parseMountType, serviceFailure } from '../../src/server/install.js';
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
    expect(parseMountType(MOUNT, '/Volumes/home')).toBe('nfs');
    expect(parseMountType(MOUNT, '/Volumes/missing')).toBeNull();
  });
});

describe('service failures', () => {
  const ok = () => ({ ok: true, out: '', err: '' });

  it('say what to check and how to skip the services', () => {
    const mac = serviceFailure(new LaunchdServices(ok, '/Users/me', 501), 'launchctl bootstrap … failed: Bootstrap failed: 5', '/Users/me/.agent-k-local/logs');
    expect(mac).toContain('launchctl bootstrap … failed: Bootstrap failed: 5\nCheck: launchctl print gui/501/com.yugabyte.agent-k-local and ');
    expect(mac).toMatch(/logs\/launchd\*\.log\./);
    expect(mac).toContain('--no-service');

    const linux = serviceFailure(new SystemdServices(ok, {}, '/home/me'), 'systemctl --user restart agent-k-local.service failed', '/home/me/.agent-k-local/logs');
    expect(linux).toContain('Check: systemctl --user status agent-k-local.service.\n');
    expect(linux).not.toContain('launchd');
  });
});
