import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ACCEPT_MS, LEARN_PROTOCOL, LINK_STALE_MS, ModLinks, POLL_HOLD_MS, type DroppedPrompt } from '../../src/server/modlink.js';

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
    expect(await caught).toBe('AgentK is stopping');
    expect(links.accepting).toBe(false);
    expect(links.accept('b', handed)).toBe(false); // its ack arrives late: not run
  });

  describe("Claude's questions (AskUserQuestion)", () => {
    const Q = [{ question: 'Which color?', header: 'Color', options: [{ label: 'Red', description: null, preview: null }], multiSelect: false }];
    let changes: string[];
    beforeEach(() => {
      changes = [];
      links = new ModLinks(undefined, () => now, (s) => changes.push(s));
    });

    it('offers them until the user answers, handing the answer to a held ask', async () => {
      expect(links.openAsk('s1', 'tu1', Q)).toBe(true);
      expect(links.openAsk('s1', 'tu1', Q)).toBe(false); // asked again after a hold: the same one
      expect(links.asking('s1')).toEqual({ id: 'tu1', questions: Q });
      const held = links.waitAsk('s1', 'tu1');
      expect(links.answerAsk('s1', 'tu1', { 0: 'Red' })).toBe(true);
      expect(await held).toEqual({ answers: { 0: 'Red' } });
      expect(links.asking('s1')).toBeNull();
      expect(links.answerAsk('s1', 'tu1', { 0: 'Red' })).toBe(false); // answered already
      // The mod didn't get it (its connection dropped): asking again gets it.
      expect(await links.waitAsk('s1', 'tu1')).toEqual({ answers: { 0: 'Red' } });
      links.endAsk('s1', 'tu1');
      expect(await links.waitAsk('s1', 'tu1')).toBe('gone');
    });

    it('ends a hold with nothing after POLL_HOLD_MS, for the mod to ask again', async () => {
      links.openAsk('s1', 'tu1', Q);
      const held = links.waitAsk('s1', 'tu1');
      vi.advanceTimersByTime(POLL_HOLD_MS);
      expect(await held).toBeNull();
      expect(links.asking('s1')).not.toBeNull();
    });

    it('drops them when the terminal answers, a new ask comes, or the mod stops asking', async () => {
      links.openAsk('s1', 'tu1', Q);
      const held = links.waitAsk('s1', 'tu1');
      links.endAsk('s1', 'other'); // not this one
      expect(links.asking('s1')?.id).toBe('tu1');
      links.endAsk('s1', 'tu1');
      expect(await held).toBe('gone');
      expect(links.answerAsk('s1', 'tu1', { 0: 'Red' })).toBe(false);

      links.openAsk('s1', 'tu2', Q);
      const old = links.waitAsk('s1', 'tu2');
      links.openAsk('s1', 'tu3', Q);
      expect(await old).toBe('gone');
      expect(links.asking('s1')?.id).toBe('tu3');

      // Not asked again (the call was interrupted, Claude died): swept once it's stale.
      now += LINK_STALE_MS;
      vi.advanceTimersByTime(2000);
      expect(links.asking('s1')).toBeNull();
    });

    it('answers a held ask with nothing while the socket closes, and with gone on a reset', async () => {
      links.openAsk('s1', 'tu1', Q);
      const held = links.waitAsk('s1', 'tu1');
      links.pause();
      expect(await held).toBeNull();
      expect(await links.waitAsk('s1', 'tu1')).toBeNull(); // not held while paused
      links.resume();
      const again = links.waitAsk('s1', 'tu1');
      links.reset('s1');
      expect(await again).toBe('gone');
      expect(links.asking('s1')).toBeNull();
    });

    it('reports changes: questions coming and going, and the link going live and stale', () => {
      links.hello('s1');
      expect(changes).toEqual(['s1']);
      links.poll('s1');
      links.openAsk('s1', 'tu1', Q);
      links.answerAsk('s1', 'tu1', { 0: 'Red' });
      expect(changes).toEqual(['s1', 's1', 's1']);
      links.endAsk('s1'); // already answered: nothing new to show
      expect(changes).toHaveLength(3);
      links.reset('s1');
      expect(changes).toHaveLength(4);
      links.hello('s2');
      changes.length = 0;
      now += LINK_STALE_MS;
      vi.advanceTimersByTime(2000);
      expect(changes).toEqual(['s2']);
      expect(links.live('s2')).toBe(false);
    });
  });

  it("knows while Claude calls Meko: reported on the first start and the last end, forgotten at the turn's end and on a reset, and not brought back by a late end", () => {
    const changes: string[] = [];
    links = new ModLinks(undefined, () => now, (s) => changes.push(s));
    links.mekoCall('s1', 'tm1', 'start'); // also the mod's first word: the link goes live
    links.mekoCall('s1', 'tm2', 'start');
    expect(links.callingMeko('s1')).toBe(true);
    expect(changes).toEqual(['s1', 's1']);
    links.mekoCall('s1', 'tm1', 'end');
    links.mekoCall('s1', 'x'.repeat(201), 'start');
    links.mekoCall('s1', 42, 'start');
    links.mekoCall('s1', 'tm3', 'begin');
    expect(changes).toHaveLength(2);
    links.mekoCall('s1', 'tm2', 'end');
    expect(links.callingMeko('s1')).toBe(false);
    expect(changes).toHaveLength(3);
    links.mekoCall('s1', 'tm4', 'start');
    links.endMeko('s1');
    expect(links.callingMeko('s1')).toBe(false);
    links.endMeko('s1'); // nothing left: nothing to report
    expect(changes).toHaveLength(5);
    links.mekoCall('s1', 'tm5', 'start');
    links.reset('s1');
    expect(links.callingMeko('s1')).toBe(false);
    expect(changes).toHaveLength(7);
    // Its end, posted as Claude exited, comes after the reset: the link stays gone.
    links.mekoCall('s1', 'tm5', 'end');
    expect(links.live('s1')).toBe(false);
    expect(changes).toHaveLength(7);
  });

  it('counts the prompts Claude holds: added mid-turn, then as the mod reports them', async () => {
    links.hello('s1');
    const held = links.poll('s1');
    const send = links.send('s1', 'Revise it.');
    const answer = await held;
    links.accept('s1', answer!.prompts[0].id);
    const id = await send;
    links.hold('s1', id);
    links.hold('s1', 'p2');
    expect(links.heldCount('s1')).toBe(2);
    links.reportHeld('s1', [id]); // p2 started, or was refused
    expect(links.heldCount('s1')).toBe(1);
    links.reportHeld('s1', 'nonsense');
    expect(links.heldCount('s1')).toBe(1);
    links.started('s1'); // a mod that doesn't report: the oldest started its turn
    expect(links.heldCount('s1')).toBe(0);
    links.hold('s1', 'p3');
    links.reset('s1');
    expect(links.heldCount('s1')).toBe(0);
  });

  it('runs learn steps only through a mod that speaks their protocol', () => {
    links.hello('old', 1);
    expect(links.live('old')).toBe(true);
    expect(links.learnReady('old')).toBe(false);
    links.hello('none');
    expect(links.learnReady('none')).toBe(false); // a hello without a protocol is the first one
    links.hello('new', LEARN_PROTOCOL);
    expect(links.learnReady('new')).toBe(true);
    now += LINK_STALE_MS;
    expect(links.learnReady('new')).toBe(false); // gone stale
  });

  it('takes back a part of a learn step the mod has not polled for, only for that step', async () => {
    links.hello('s1', LEARN_PROTOCOL);
    links.queueLearn('s1', { id: 'L1', kind: 'extract', prompt: 'What did you learn?' });
    links.withdrawLearn('s1', 'L2'); // another step's: left alone
    links.withdrawLearn('s1', 'L1');
    links.queueToast('s1', "AgentK couldn't save what this session learned.");
    expect(await links.poll('s1')).toEqual({ prompts: [], toast: "AgentK couldn't save what this session learned." });
  });

  it("knows a mod's protocol from its polls, after a server restart lost its hello", () => {
    void links.poll('s1'); // the running mod's next poll: no hello since the restart
    expect(links.learnReady('s1')).toBe(false);
    links.reportProtocol('s1', LEARN_PROTOCOL);
    expect(links.learnReady('s1')).toBe(true);
    links.reportProtocol('s1', undefined); // a mod from before learn steps names none: nothing changes
    expect(links.learnReady('s1')).toBe(true);
  });

  it('hands a part of a learn step and a line to show to a held poll, once, beside the prompts', async () => {
    links.hello('s1', LEARN_PROTOCOL);
    const held = links.poll('s1');
    links.queueLearn('s1', { id: 'L1', kind: 'extract', prompt: 'What did you learn?' });
    expect(await held).toEqual({ prompts: [], learn: { id: 'L1', kind: 'extract', prompt: 'What did you learn?' } });
    links.queueLearn('s1', { id: 'L1', kind: 'check', prompt: 'Check these.', model: 'haiku' });
    links.queueToast('s1', 'Saved 2 memories.');
    const send = links.send('s1', 'Next prompt.');
    expect(await links.poll('s1')).toEqual({
      prompts: [{ id: expect.any(String), text: 'Next prompt.' }],
      learn: { id: 'L1', kind: 'check', prompt: 'Check these.', model: 'haiku' },
      toast: 'Saved 2 memories.',
    });
    const next = links.poll('s1');
    vi.advanceTimersByTime(POLL_HOLD_MS);
    expect(await next).toBeNull();
    vi.advanceTimersByTime(ACCEPT_MS);
    await expect(send).rejects.toThrow();
  });
});
