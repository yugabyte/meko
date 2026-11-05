import { describe, expect, it } from 'vitest';
import { formatStatus, upFor, type StatusFacts } from '../../src/server/statusview.js';

const NOW = Date.parse('2026-11-03T12:00:00.000Z');

/** A running AgentK, its server up 2h4m, with `o` changed. */
function facts(o: Partial<StatusFacts> = {}): StatusFacts {
  return {
    version: '0.20261103.13',
    probe: { ok: true, version: '0.20261103.13', pid: 39527, startedAt: '2026-11-03T09:56:00.000Z' },
    leasePid: 39527,
    host: { up: true, pid: 39531, sessions: 0, running: 0 },
    services: { kind: 'launchd', units: [
      { id: 'com.yugabyte.me_koder', installed: true, running: true, text: 'running' },
      { id: 'com.yugabyte.me_koder.host', installed: true, running: true, text: 'running' },
    ] },
    webUrl: 'http://127.0.0.1:7777/ext/agentk/',
    home: '/home/alice/.meko/extensions/agentk',
    logs: '/home/alice/.meko/extensions/agentk/logs/server.log',
    now: NOW,
    ...o,
  };
}

describe("me_koder status, shaped like meko status", () => {
  it('opens with a marked headline, then a row per part, then the links in a box like Meko\'s', () => {
    expect(formatStatus(facts())).toEqual([
      '✓ AgentK 0.20261103.13 is running (pid 39527, up 2h4m0s)',
      '  ✓ Server          healthy',
      '  ✓ Session host    running (pid 39531), 0 sessions',
      '  ✓ Service         launchd',
      '',
      '┌──────────────────────────────────────────────────────────────┐',
      '│ AgentK 0.20261103.13                                         │',
      '├──────────────────────────────────────────────────────────────┤',
      '│ Web UI   http://127.0.0.1:7777/ext/agentk/                   │',
      '│ Home     /home/alice/.meko/extensions/agentk                 │',
      '│ Logs     /home/alice/.meko/extensions/agentk/logs/server.log │',
      '└──────────────────────────────────────────────────────────────┘',
    ]);
  });

  it('says when it is stopped, with the sessions the host keeps, and how to start it', () => {
    const lines = formatStatus(facts({
      probe: { ok: false, error: 'connect ENOENT' },
      leasePid: null,
      host: { up: true, pid: 39531, sessions: 2, running: 1 },
      services: { kind: 'systemd', units: [
        { id: 'me_koder.service', installed: true, running: false, text: 'inactive' },
        { id: 'me_koder-host.service', installed: true, running: false, text: 'inactive' },
      ] },
    }));
    expect(lines[0]).toBe("✗ AgentK 0.20261103.13 isn't running");
    expect(lines).toContain('  ✓ Session host    running (pid 39531), 2 sessions (1 running)');
    expect(lines).toContain('  ! Service         systemd, stopped');
    expect(lines.some((l) => l.includes('Server'))).toBe(false);
    expect(lines.at(-1)).toBe('  Start it: me_koder start');
  });

  it('warns about a server that holds the lease but is slow to answer, and a host that is down', () => {
    const lines = formatStatus(facts({ probe: { ok: false, error: 'timed out' }, host: { up: false } }));
    expect(lines[0]).toBe("! AgentK 0.20261103.13 is running (pid 39527), but isn't answering");
    expect(lines).toContain('  ! Server          not answering (timed out)');
    expect(lines).toContain('  ! Session host    not running');
    expect(lines).not.toContain('  Start it: me_koder start');
  });

  it('goes without the uptime from a server before it, and says what is wrong with the services', () => {
    const lines = formatStatus(facts({
      probe: { ok: true, version: '0.20261103.12' },
      host: { error: 'EACCES' },
      services: { kind: 'launchd', units: [
        { id: 'com.yugabyte.me_koder', installed: true, running: true, text: 'running' },
        { id: 'com.yugabyte.me_koder.host', installed: false, running: false, text: 'not installed' },
      ] },
      webUrl: null,
      webWhy: "Meko isn't running: start it with meko start",
    }));
    expect(lines[0]).toBe('✓ AgentK 0.20261103.13 is running (pid 39527)');
    expect(lines).toContain('  ! Session host    not answering (EACCES)');
    expect(lines).toContain('  ! Service         launchd: com.yugabyte.me_koder.host not installed');
    expect(lines.some((l) => l.startsWith("│ Web UI   none (Meko isn't running: start it with meko start)"))).toBe(true);
    expect(formatStatus(facts({ services: 'other' }))).toContain('  ! Service         none for this home (the installed services run another one)');
    expect(formatStatus(facts({ services: null })).some((l) => l.includes('Service'))).toBe(false);
  });

  it("colors the rows' marks, never the headline's, which Meko reads", () => {
    const lines = formatStatus(facts(), (_k, m) => `<${m}>`);
    expect(lines[0].startsWith('✓ AgentK')).toBe(true);
    expect(lines[1]).toBe('  <✓> Server          healthy');
  });

  it('writes the uptime as Go writes a duration, as meko status does', () => {
    expect(upFor(33_000)).toBe('33s');
    expect(upFor(243_000)).toBe('4m3s');
    expect(upFor((2 * 3600 + 4 * 60) * 1000)).toBe('2h4m0s');
    expect(upFor(-5)).toBe('0s');
  });
});
