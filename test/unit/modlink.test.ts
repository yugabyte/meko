import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ACCEPT_MS, LINK_STALE_MS, ModLinks, POLL_HOLD_MS } from '../../src/server/modlink.js';

describe('the link to AgentK\'s mod', () => {
  let now = 1_000_000;
  let dropped: [string, string][];
  let links: ModLinks;
  beforeEach(() => {
    vi.useFakeTimers();
    now = 1_000_000;
    dropped = [];
    links = new ModLinks((s, r) => dropped.push([s, r]), () => now);
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it('is live while the mod polls, and goes stale when it stops', async () => {
    expect(links.live('s1')).toBe(false);
    expect(links.hello('s1')).toBe(true);
    expect(links.hello('s1')).toBe(false); // said once per launch
    expect(links.live('s1')).toBe(true);
    const held = links.poll('s1');
    now += LINK_STALE_MS * 3; // a held poll keeps it live however long it's held
    expect(links.live('s1')).toBe(true);
    vi.advanceTimersByTime(POLL_HOLD_MS);
    expect(await held).toBeNull();
    expect(links.live('s1')).toBe(true); // just answered: the mod polls again right away
    now += LINK_STALE_MS;
    expect(links.live('s1')).toBe(false);
    expect(links.live('other')).toBe(false);
  });

  it('answers a held poll with a prompt, and resolves the send on the mod\'s ack', async () => {
    links.hello('s1');
    const held = links.poll('s1');
    let sent = false;
    const send = links.send('s1', 'Implement the design.').then(() => (sent = true));
    const answer = await held;
    expect(answer?.prompts).toEqual([{ id: expect.any(String), text: 'Implement the design.' }]);
    await Promise.resolve();
    expect(sent).toBe(false); // not until the mod says it took it
    links.ack('s1', answer!.prompts[0].id, 'accepted');
    await send;
    expect(sent).toBe(true);
    // Handed once: the next poll waits.
    const next = links.poll('s1');
    vi.advanceTimersByTime(POLL_HOLD_MS);
    expect(await next).toBeNull();
  });

  it('answers a poll at once when a prompt is already waiting', async () => {
    links.hello('s1');
    const send = links.send('s1', 'Open the PR.');
    const answer = await links.poll('s1');
    expect(answer?.prompts.map((p) => p.text)).toEqual(['Open the PR.']);
    links.ack('s1', answer!.prompts[0].id, 'accepted');
    await expect(send).resolves.toBeUndefined();
  });

  it('fails a send the mod never acks, and never hands that prompt out again', async () => {
    links.hello('s1');
    const send = links.send('s1', 'Fix the review.');
    const caught = send.catch((e: Error) => e.message);
    const answer = await links.poll('s1');
    expect(answer?.prompts).toHaveLength(1);
    vi.advanceTimersByTime(ACCEPT_MS);
    expect(await caught).toMatch(/didn't take the prompt/);
    links.ack('s1', answer!.prompts[0].id, 'accepted'); // too late: nothing to resolve
    const next = links.poll('s1');
    vi.advanceTimersByTime(POLL_HOLD_MS);
    expect(await next).toBeNull();
  });

  it('passes on a prompt another plugin refused', () => {
    links.hello('s1');
    links.ack('s1', 'p1', 'dropped', 'not now');
    links.ack('s1', 'p2', 'dropped');
    expect(dropped).toEqual([['s1', 'not now'], ['s1', 'no reason given']]);
  });

  it('answers an older poll with nothing when a newer one comes (a reloaded module)', async () => {
    links.hello('s1');
    const old = links.poll('s1');
    const fresh = links.poll('s1');
    expect(await old).toBeNull();
    const send = links.send('s1', 'Go on.');
    const answer = await fresh;
    expect(answer?.prompts.map((p) => p.text)).toEqual(['Go on.']);
    links.ack('s1', answer!.prompts[0].id, 'accepted');
    await send;
  });

  it('lets go of a poll whose connection closed, keeping the prompts for the next one', async () => {
    links.hello('s1');
    const closed = new AbortController();
    const held = links.poll('s1', closed.signal);
    closed.abort();
    expect(await held).toBeNull();
    const send = links.send('s1', 'Still here?');
    const answer = await links.poll('s1');
    expect(answer?.prompts.map((p) => p.text)).toEqual(['Still here?']);
    links.ack('s1', answer!.prompts[0].id, 'accepted');
    await send;
  });

  it('on a reset (Claude relaunched or ended), fails what was on its way and forgets the mod', async () => {
    links.hello('s1');
    const held = links.poll('s1');
    const caught = links.send('s1', 'Lost.').catch((e: Error) => e.message);
    const answer = await held;
    expect(answer?.prompts).toHaveLength(1);
    links.reset('s1', 'Claude exited before taking the prompt');
    expect(await caught).toBe('Claude exited before taking the prompt');
    expect(links.live('s1')).toBe(false);
    expect(links.hello('s1')).toBe(true); // the next launch's mod says hello afresh
  });

  it('answers every held poll when the server stops', async () => {
    links.hello('a');
    links.hello('b');
    const polls = [links.poll('a'), links.poll('b')];
    links.closeAll();
    expect(await Promise.all(polls)).toEqual([null, null]);
  });
});
