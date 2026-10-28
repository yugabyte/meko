import { rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { readJsonl, tempDir } from '../helpers.js';

describe('readJsonl (the test helper over the fakes’ logs)', () => {
  let dir: string;
  beforeEach(() => {
    dir = tempDir('akl-jsonl-');
  });
  afterEach(() => rmSync(dir, { recursive: true, force: true }));

  // A last line without its newline is an append caught halfway: it's left for the next read.
  it.each([
    ['a file not written yet', null, []],
    ['an empty file', '', []],
    ['complete records', '{"a":1}\n{"b":2}\n', [{ a: 1 }, { b: 2 }]],
    ['a record still being appended', '{"a":1}\n{"b":"tw', [{ a: 1 }]],
    ['a first record still being appended', '{"args":["--resume"', []],
  ])('reads %s', (_what, text, records) => {
    const file = join(dir, 'argv.jsonl');
    if (text !== null) writeFileSync(file, text);
    expect(readJsonl(file)).toEqual(records);
  });
});
