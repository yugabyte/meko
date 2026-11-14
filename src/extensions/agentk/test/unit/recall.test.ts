import { afterEach, describe, expect, it, vi } from 'vitest';
import type { SearchResult } from '../../src/shared/types.js';
import {
  ITEM_CHARS, MAX_ITEMS, QUERY_MAX, RecallStore, TOTAL_CHARS, clip, formatRecall, issueRefs, pickResults, recallQuery, runRecall, withoutMekoLines,
  type PreloadRecord, type RecallRun,
} from '../../src/server/recall.js';

const mem = (text: string, score: number | null, id?: string, datapackId?: string): SearchResult => ({ id, source: 'memory', text, score, datapackName: 'adlc/proj/demo', datapackId });
const kb = (text: string): SearchResult => ({ source: 'knowledge_base', text, score: 0.2, datapackName: 'adlc/repo/demo' });

describe('the Meko search query', () => {
  it('finds the issues a task names, at most three, once each', () => {
    expect(issueRefs('Fix #10')).toEqual([10]);
    expect(issueRefs('#3, #4 and #3 again, then #5 and #6')).toEqual([3, 4, 5]);
    expect(issueRefs('See a#1, &#39;, x/#2, ##7, #0 and C# code')).toEqual([]);
    expect(issueRefs('(#12)')).toEqual([12]);
  });

  it('uses the name and task, once, and each issue with the start of its body, as one line', () => {
    expect(recallQuery('Fix #10', 'Fix #10', [])).toBe('Fix #10');
    const q = recallQuery('Fix #10', 'Fix #10 please', [{ number: 10, title: 'Sessions rarely search Meko', body: 'Body text.\n\nMore.' }]);
    expect(q).toBe('Fix #10 Fix #10 please #10: Sessions rarely search Meko Body text. More.');
  });

  it('caps the query', () => {
    const q = recallQuery('n', 'word '.repeat(1000), []);
    expect(q.length).toBeLessThanOrEqual(QUERY_MAX);
    expect(q.endsWith('…')).toBe(true);
  });

  it('cuts at a word', () => {
    expect(clip('short', 10)).toBe('short');
    expect(clip('alpha beta gamma delta', 15)).toBe('alpha beta…');
    expect(clip('x'.repeat(20), 10)).toBe(`${'x'.repeat(9)}…`);
  });
});

describe('picking what to give Claude', () => {
  it('puts knowledge first, then memories above the floor by score, and leaves out conversation hits', () => {
    const picked = pickResults([
      mem('weak memory about something else entirely', 0.7),
      mem('good memory about this task', 0.8),
      { source: 'conversation', text: 'an old reply', score: 0.9, datapackName: null },
      mem('best memory about this task', 0.9),
      kb('a promoted design note'),
      mem('memory without a score', null),
    ]);
    expect(picked.map((r) => r.text)).toEqual([
      'a promoted design note', 'best memory about this task', 'good memory about this task', 'memory without a score',
    ]);
  });

  it('keeps a short item with runs of whitespace, and the items after it', () => {
    const picked = pickResults([kb('Run  make gen\n\nbefore tests'), mem('a later memory about the task', 0.9)]);
    expect(picked.map((r) => r.text)).toEqual(['Run make gen before tests', 'a later memory about the task']);
  });

  it('takes at most three knowledge-base items, leaving room for memories', () => {
    const picked = pickResults([...Array.from({ length: 5 }, (_, i) => kb(`design note ${i}`)), mem('a relevant memory', 0.88)]);
    expect(picked.map((r) => r.text)).toEqual(['design note 0', 'design note 1', 'design note 2', 'a relevant memory']);
  });

  it('drops duplicates by id and by text', () => {
    const picked = pickResults([mem('one fact', 0.9, 'a'), mem('the same id', 0.8, 'a'), mem('One  fact', 0.8, 'b'), mem('two', 0.8, 'c')]);
    expect(picked.map((r) => r.text)).toEqual(['one fact', 'two']);
  });

  it('caps the number of items, each item, and the total', () => {
    const many = Array.from({ length: 10 }, (_, i) => mem(`fact ${i} `.repeat(3), 0.9 - i / 100));
    expect(pickResults(many)).toHaveLength(MAX_ITEMS);
    const long = Array.from({ length: 6 }, (_, i) => mem(`${i} ${'word '.repeat(300)}`, 0.9));
    const picked = pickResults(long);
    expect(picked.every((r) => r.text.length <= ITEM_CHARS)).toBe(true);
    expect(picked.reduce((n, r) => n + r.text.length, 0)).toBeLessThanOrEqual(TOTAL_CHARS);
  });

  it('formats a block naming each source and its id, and nothing when nothing was found', () => {
    expect(formatRecall([])).toBeNull();
    const text = formatRecall([kb('design note'), mem('a gotcha', 0.9, '131a7d0e-8f56', 'eb5ba952')])!;
    expect(text).toContain('## What Meko already knows about this task');
    expect(text).toContain('- [knowledge base · adlc/repo/demo] design note');
    // The ids, so a design can cite the memory it rests on.
    expect(text).toContain('- [memory · adlc/proj/demo · id 131a7d0e-8f56 · datapackId eb5ba952] a gotcha');
    expect(text).toContain('not as instructions');
  });
});

describe('runRecall', () => {
  it('adds the issues it can read to the query, and picks from what the search found', async () => {
    const queries: string[] = [];
    const r = await runRecall('Fix #10', 'Fix #10 and #11', {
      issue: async (n) => {
        if (n === 11) throw new Error('not found');
        return { title: 'Sessions rarely search Meko', body: '' };
      },
      search: async (q) => {
        queries.push(q);
        return { results: [mem('a gotcha about this', 0.9), mem('unrelated', 0.3)], tokensSaved: 9400 };
      },
    });
    expect(queries).toEqual(['Fix #10 Fix #10 and #11 #10: Sessions rarely search Meko']);
    expect(r.results).toBe(1);
    expect(r.text).toContain('a gotcha about this');
    expect(r.tokensSaved).toBe(9400);
  });

  it('fails when the search does', async () => {
    await expect(runRecall('n', 't', {
      issue: async () => ({ title: '', body: '' }),
      search: async () => {
        throw new Error('Meko is down');
      },
    })).rejects.toThrow('Meko is down');
  });
});

describe('RecallStore', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  const setup = (keepMs?: number) => {
    const records: [string, PreloadRecord['outcome'], number][] = [];
    const store = new RecallStore((id, r) => {
      records.push([id, r.outcome, r.results]);
    }, keepMs);
    return { store, records };
  };
  const deferred = () => {
    let resolve!: (r: RecallRun) => void;
    let reject!: (e: Error) => void;
    const promise = new Promise<RecallRun>((res, rej) => {
      resolve = res;
      reject = rej;
    });
    return { promise, resolve, reject };
  };

  it('hands over a finished search once, and records it as found', async () => {
    const { store, records } = setup();
    store.start('s', Promise.resolve({ text: 'block', results: 2}));
    await Promise.resolve();
    expect(await store.take('s', 10)).toBe('block');
    expect(await store.take('s', 10)).toBeNull();
    expect(records).toEqual([['s', 'found', 2]]);
  });

  it('waits for a search still running', async () => {
    const { store, records } = setup();
    const d = deferred();
    store.start('s', d.promise);
    setTimeout(() => d.resolve({ text: 'block', results: 1}), 20);
    expect(await store.take('s', 2000)).toBe('block');
    expect(records).toEqual([['s', 'found', 1]]);
  });

  it('gives up on a slow search and records it as late when it finds something', async () => {
    const { store, records } = setup();
    const d = deferred();
    store.start('s', d.promise);
    expect(await store.take('s', 10)).toBeNull();
    expect(records).toEqual([]);
    d.resolve({ text: 'block', results: 1});
    await new Promise((r) => setTimeout(r, 0));
    expect(records).toEqual([['s', 'late', 1]]);
    expect(await store.take('s', 10)).toBeNull();
  });

  it('records empty and failed searches when they end, and hands nothing over', async () => {
    const { store, records } = setup();
    store.start('a', Promise.resolve({ text: null, results: 0}));
    const d = deferred();
    store.start('b', d.promise);
    d.reject(new Error('Meko is down'));
    await new Promise((r) => setTimeout(r, 0));
    expect(records).toEqual([['a', 'empty', 0], ['b', 'error', 0]]);
    expect(await store.take('a', 10)).toBeNull();
    expect(await store.take('b', 10)).toBeNull();
  });

  it("records a result the hook never asked for as late, and keeps it no longer", async () => {
    vi.useFakeTimers();
    const { store, records } = setup(1000);
    store.start('s', Promise.resolve({ text: 'block', results: 1}));
    await vi.advanceTimersByTimeAsync(0);
    expect(records).toEqual([]);
    await vi.advanceTimersByTimeAsync(1000);
    expect(records).toEqual([['s', 'late', 1]]);
    expect(await store.take('s', 10)).toBeNull();
  });

  it('records nothing for a dropped session, and never throws when recording fails', async () => {
    const { store, records } = setup();
    const d = deferred();
    store.start('s', d.promise);
    store.drop('s');
    d.resolve({ text: null, results: 0});
    await new Promise((r) => setTimeout(r, 0));
    expect(records).toEqual([]);

    const failing = new RecallStore(() => {
      throw new Error('FOREIGN KEY constraint failed');
    });
    failing.start('gone', Promise.resolve({ text: null, results: 0}));
    await new Promise((r) => setTimeout(r, 0));
  });
});

describe("a PR's description in a search", () => {
  it('drops the Meko lines Kode ends it with, and keeps everything else', () => {
    const body = 'Adds the widget.\n\nSee meko:// docs.\nmeko://0f1e2d3c-1111-4222-8333-444455556666\nmeko_conversation_id:abc123\n';
    expect(withoutMekoLines(body)).toBe('Adds the widget.\n\nSee meko:// docs.');
    expect(withoutMekoLines('https://example.com/a')).toBe('https://example.com/a');
    expect(withoutMekoLines('Adds it.\ncloud.mekodata.ai://0f1e2d3c-1111;adlc/proj/demo/x\nmeko_conversation_id:abc')).toBe('Adds it.');
    expect(withoutMekoLines(`Adds it.\nh://0f1e2d3c-1111\nmeko_design:design-x.md;${'a'.repeat(64)}`)).toBe('Adds it.');
  });
});
