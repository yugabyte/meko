import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ACCEPT_MS, LINK_STALE_MS, ModLinks, POLL_HOLD_MS, type DroppedPrompt } from '../../src/server/modlink.js';

describe('the link to AgentK\'s mod', () => {
  let now = 1_000_000;
  let dropped: [string, DroppedPrompt][];
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
    expect(links.accept('s1', answer!.prompts[0].id)).toBe(true);
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
    expect(links.accept('s1', answer!.prompts[0].id)).toBe(true);
    await expect(send).resolves.toBeUndefined();
  });

  it('fails a send the mod never acks, tells a late ack not to run it, and never hands it out again', async () => {
    links.hello('s1');
    const send = links.send('s1', 'Fix the review.');
    const caught = send.catch((e: Error) => e.message);
    const answer = await links.poll('s1');
    expect(answer?.prompts).toHaveLength(1);
    vi.advanceTimersByTime(ACCEPT_MS);
    expect(await caught).toMatch(/didn't take the prompt/);
    // Too late: the caller already has the prompt back, so the mod must not submit it.
    expect(links.accept('s1', answer!.prompts[0].id)).toBe(false);
    expect(links.accept('s1', 'unknown')).toBe(false);
    expect(links.accept('other', answer!.prompts[0].id)).toBe(false);
    const next = links.poll('s1');
    vi.advanceTimersByTime(POLL_HOLD_MS);
    expect(await next).toBeNull();
  });

  it('passes on a prompt another plugin refused, with its text', async () => {
    links.hello('s1');
    const send = links.send('s1', 'Open the PR.\n\nThen wait for review.');
    const answer = await links.poll('s1');
    const id = answer!.prompts[0].id;
    expect(links.accept('s1', id)).toBe(true);
    await send;
    links.dropped('s1', id, 'not now', true);
    links.dropped('s1', id, ' ', 'yes'); // its text is only kept for the first report
    expect(dropped).toEqual([
      ['s1', { text: 'Open the PR.\n\nThen wait for review.', reason: 'not now', filled: true }],
      ['s1', { text: null, reason: 'no reason given', filled: false }],
    ]);
  });

  it('while the hook socket closes (a rebind), answers held polls at once and holds none until it resumes', async () => {
    links.hello('s1');
    const held = links.poll('s1');
    links.pause();
    expect(links.accepting).toBe(false);
    expect(await held).toBeNull();
    expect(await links.poll('s1')).toBeNull(); // not held
    expect(links.live('s1')).toBe(true); // the link itself stays
    links.resume();
    const next = links.poll('s1');
    const send = links.send('s1', 'After the rebind.');
    const answer = await next;
    expect(answer?.prompts.map((p) => p.text)).toEqual(['After the rebind.']);
    expect(links.accept('s1', answer!.prompts[0].id)).toBe(true);
    await send;
  });

  it('answers an older poll with nothing when a newer one comes (a reloaded module)', async () => {
    links.hello('s1');
    const old = links.poll('s1');
    const fresh = links.poll('s1');
    expect(await old).toBeNull();
    const send = links.send('s1', 'Go on.');
    const answer = await fresh;
    expect(answer?.prompts.map((p) => p.text)).toEqual(['Go on.']);
    expect(links.accept('s1', answer!.prompts[0].id)).toBe(true);
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
    expect(links.accept('s1', answer!.prompts[0].id)).toBe(true);
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

  it('answers every held poll when the server stops, and fails what was on its way', async () => {
    links.hello('a');
    links.hello('b');
    const polls = [links.poll('a'), links.poll('b')];
    const caught = links.send('b', 'Stopping.').catch((e: Error) => e.message);
    const handed = (await polls[1])!.prompts[0].id;
    links.closeAll();
    expect(await polls[0]).toBeNull();
    expect(await caught).toBe('AgentK Local is stopping');
    expect(links.accepting).toBe(false);
    expect(links.accept('b', handed)).toBe(false); // its ack arrives late: not run
  });
});
