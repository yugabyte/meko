import { appendFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { promptInTranscript, transcriptMarkOf } from '../../src/server/claude.js';
import { tempDir } from '../helpers.js';

describe('whether a prompt started its turn, from the Claude Code transcript written since it was sent', () => {
  let dir = '';
  afterEach(() => rmSync(dir, { recursive: true, force: true }));

  const PROMPT = 'Open a pull request for "this work".\n\nThen report back.';
  const user = (text: string) => JSON.stringify({ type: 'user', message: { role: 'user', content: text } }) + '\n';

  it.each([
    ['its user message was written', [user(PROMPT)], true],
    ['only metadata naming it was written (as at an exit)', [JSON.stringify({ type: 'last-prompt', lastPrompt: PROMPT }) + '\n'], false],
    ['nothing was written', [], false],
    ['another prompt ran', [user('Something else.')], false],
  ])('%s', (_what, lines, started) => {
    dir = tempDir('akl-mark-');
    const file = join(dir, 't.jsonl');
    writeFileSync(file, user(PROMPT)); // an earlier turn with the same text: before the mark, so not this one
    const mark = transcriptMarkOf(file);
    for (const l of lines) appendFileSync(file, l);
    expect(promptInTranscript(mark, PROMPT)).toBe(started);
  });

  it('counts a transcript not written yet as not started, and one replaced or removed since as unknown, so started', () => {
    dir = tempDir('akl-mark-');
    const file = join(dir, 't.jsonl');
    expect(promptInTranscript(transcriptMarkOf(file), PROMPT)).toBe(false);
    writeFileSync(file, user('a long earlier turn'));
    const mark = transcriptMarkOf(file);
    writeFileSync(file, '');
    expect(promptInTranscript(mark, PROMPT)).toBe(true);
    rmSync(file);
    expect(promptInTranscript(mark, PROMPT)).toBe(true);
  });
});
