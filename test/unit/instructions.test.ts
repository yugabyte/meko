import { describe, expect, it } from 'vitest';
import { checkInstructions, openPrInstructions } from '../../src/server/lifecycle.js';
import { DEFAULT_PROMPTS, render } from '../../src/server/prompts.js';
import { INSTRUCTIONS_MAX, type Session } from '../../src/shared/types.js';
import { instructionsBlocker, promptBlocker } from '../../src/web/util.js';

describe('instructions for a move or Open PR', () => {
  it('reads blank or missing instructions as none, and trims the rest', () => {
    expect(checkInstructions(undefined)).toBe('');
    expect(checkInstructions(null)).toBe('');
    expect(checkInstructions('  \n ')).toBe('');
    expect(checkInstructions('  Open it as a draft.\n')).toBe('Open it as a draft.');
    expect(checkInstructions(` ${'x'.repeat(INSTRUCTIONS_MAX)} `)).toHaveLength(INSTRUCTIONS_MAX);
  });

  it('refuses instructions that are not text or are too long', () => {
    expect(() => checkInstructions(3)).toThrow(expect.objectContaining({ status: 400, message: 'Instructions must be text.' }));
    expect(() => checkInstructions('x'.repeat(INSTRUCTIONS_MAX + 1))).toThrow(
      expect.objectContaining({ status: 400, message: 'Instructions can be at most 10,000 characters; these have 10,001.' }),
    );
  });

  it("reads an open-pr body, which is optional", () => {
    expect(openPrInstructions(undefined)).toBe('');
    expect(openPrInstructions({})).toBe('');
    expect(openPrInstructions({ instructions: ' draft ' })).toBe('draft');
    expect(() => openPrInstructions('draft')).toThrow(expect.objectContaining({ status: 400 }));
    expect(() => openPrInstructions([])).toThrow(expect.objectContaining({ status: 400 }));
    expect(() => openPrInstructions({ instructions: false })).toThrow(expect.objectContaining({ status: 400 }));
  });

  it("puts the user's text in the instructions prompt as is", () => {
    const text = render(DEFAULT_PROMPTS.instructions, { instructions: 'Keep {{task}} as is.', task: 'the task' });
    expect(text).toMatch(/^The user added these instructions for this step in AgentK Local\. Follow them; where they conflict with the instructions above, follow the user's\./);
    expect(text.endsWith('\n\nKeep {{task}} as is.')).toBe(true);
  });

  it('says in the browser when the text is too long, as the server would', () => {
    expect(instructionsBlocker('')).toBeNull();
    expect(instructionsBlocker('x'.repeat(INSTRUCTIONS_MAX))).toBeNull();
    expect(instructionsBlocker('x'.repeat(INSTRUCTIONS_MAX + 1))).toBe('Instructions can be at most 10,000 characters; these have 10,001.');
  });

  it('says in the browser when Claude can’t take a typed prompt', () => {
    const s = (over: Partial<Session>) => ({ status: 'idle', statusDetail: null, ...over }) as Session;
    expect(promptBlocker(s({}))).toBeNull();
    expect(promptBlocker(s({ status: 'needs_input', statusDetail: 'Design ready for your approval' }))).toBeNull();
    expect(promptBlocker(s({ status: 'needs_input', statusDetail: 'Permission needed for Bash' }))).toBe('AgentK is waiting on you in the terminal. Answer it first.');
    expect(promptBlocker(s({ status: 'exited' }))).toBe('Claude has exited. Resume the session first.');
  });
});
