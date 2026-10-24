import { describe, expect, it } from 'vitest';
import { MekoCalls } from '../../src/server/mekocalls.js';

describe('Meko calls under way, as hooks report them', () => {
  it('opens a call at its start and closes it at its end, per session', () => {
    const calls = new MekoCalls();
    calls.start('s1', '@meko/context_search');
    expect(calls.active('s1')).toBe(true);
    expect(calls.active('s2')).toBe(false);
    calls.end('s1', '@meko/context_search');
    expect(calls.active('s1')).toBe(false);
  });

  it('counts a key started twice (two Kiro calls of one tool), and ignores an end that never started', () => {
    const calls = new MekoCalls();
    calls.end('s1', 'k');
    expect(calls.active('s1')).toBe(false);
    calls.start('s1', 'k');
    calls.start('s1', 'k');
    calls.end('s1', 'k');
    expect(calls.active('s1')).toBe(true);
    calls.end('s1', 'k');
    calls.end('s1', 'k');
    expect(calls.active('s1')).toBe(false);
  });

  it('clears a session at once (the turn ended), and keeps no more than its cap or a key too long', () => {
    const calls = new MekoCalls();
    calls.start('s1', 'x'.repeat(201));
    expect(calls.active('s1')).toBe(false);
    for (let i = 0; i < 25; i++) calls.start('s1', `k${i}`);
    for (let i = 0; i < 20; i++) calls.end('s1', `k${i}`);
    // Past the cap, the extra starts were dropped, so their ends find nothing.
    expect(calls.active('s1')).toBe(false);
    calls.start('s1', 'a');
    calls.start('s2', 'b');
    calls.clear('s1');
    expect([calls.active('s1'), calls.active('s2')]).toEqual([false, true]);
  });
});
