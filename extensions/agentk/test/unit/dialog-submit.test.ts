import { describe, expect, it } from 'vitest';
import { submitButton } from '../../src/web/components/common.js';
import { questionsSubmitButton } from '../../src/web/components/DesignQuestions.js';
import { isSubmitKey } from '../../src/web/util.js';

/** Just enough of an element for submitButton: its selectors (tag, classes, attributes, `.a b` descendants) and classList. */
class El {
  parent: El | null = null;
  readonly children: El[];
  constructor(
    readonly tag: string,
    readonly attrs: Record<string, string> = {},
    children: El[] = [],
  ) {
    this.children = children;
    for (const c of children) c.parent = this;
  }
  readonly classList = { contains: (c: string) => (this.attrs.class ?? '').split(' ').includes(c) };
  private is(simple: string): boolean {
    const m = /^(\w*)((?:\.[\w-]+)*)((?:\[[\w-]+\])*)(?::not\(\[(\w+)="(\w+)"\]\))?$/.exec(simple);
    if (!m) throw new Error(`unsupported selector ${simple}`);
    const [, tag, classes, attrs, notAttr, notValue] = m;
    return (
      (!tag || tag === this.tag) &&
      classes.split('.').filter(Boolean).every((c) => this.classList.contains(c)) &&
      [...attrs.matchAll(/\[([\w-]+)\]/g)].every(([, a]) => a in this.attrs) &&
      (!notAttr || this.attrs[notAttr] !== notValue)
    );
  }
  private matches(selector: string): boolean {
    const [outer, inner] = selector.includes(' ') ? selector.split(' ') : [null, selector];
    if (!this.is(inner)) return false;
    for (let p = this.parent; p; p = p.parent) if (!outer || p.is(outer)) return true;
    return !outer;
  }
  private all(): El[] {
    return this.children.flatMap((c) => [c, ...c.all()]);
  }
  querySelector(selector: string): El | null {
    return this.all().find((e) => e.matches(selector)) ?? null;
  }
  closest(selector: string): El | null {
    if (this.is(selector)) return this;
    return this.parent?.closest(selector) ?? null;
  }
}

const el = (tag: string, attrs: Record<string, string> = {}, ...children: El[]) => new El(tag, attrs, children);
const pick = (root: El, focused: El) => submitButton(root as unknown as ParentNode, focused as unknown as Element) as unknown as El | null;

describe('Ctrl+Enter in a dialog', () => {
  it.each([
    [{ key: 'Enter', ctrlKey: true, metaKey: false, defaultPrevented: false }, true],
    [{ key: 'Enter', ctrlKey: false, metaKey: true, defaultPrevented: false }, true],
    [{ key: 'Enter', ctrlKey: false, metaKey: false, defaultPrevented: false }, false],
    [{ key: 'a', ctrlKey: true, metaKey: false, defaultPrevented: false }, false],
    // Another PR's box takes Enter as Find, so the dialog doesn't start on the PR picked before.
    [{ key: 'Enter', ctrlKey: true, metaKey: false, defaultPrevented: true }, false],
  ])('takes Ctrl or ⌘ plus Enter only, unless a field took it: %o', (e, sends) => {
    expect(isSubmitKey(e)).toBe(sends);
  });

  it('presses the main button of the form typed in, of the footer, or the one marked', () => {
    // Move to another stage: the task box's form has only stage buttons; Move is in the footer.
    const move = el('button', { class: 'btn primary' });
    const task = el('textarea');
    const moveDialog = el('div', {}, el('form', {}, el('button', { type: 'button', class: 'on' }), task), el('div', { class: 'dialog-ft' }, el('button', { class: 'btn' }), move));
    expect(pick(moveDialog, task)).toBe(move);

    // Settings: each form has its own Save; the TOTP code's form, whose submit is Turn off, gets none.
    const save = el('button', { class: 'btn primary' });
    const name = el('input');
    const code = el('input');
    const settings = el('div', {}, el('form', {}, el('button', { type: 'button', class: 'btn small primary' }), name, save), el('form', {}, code, el('button', { class: 'btn danger' })));
    expect(pick(settings, name)).toBe(save);
    expect(pick(settings, code)).toBeNull();
    // Nor does it get a footer's primary button.
    const turnOff = el('input');
    expect(pick(el('div', {}, el('form', {}, turnOff, el('button', { class: 'btn danger' })), el('div', { class: 'dialog-ft' }, el('button', { class: 'btn primary' }))), turnOff)).toBeNull();

    // AgentK is asking: no form or footer, an Answer marked as the dialog's.
    const answer = el('button', { class: 'btn small primary', 'data-submit': '' });
    const other = el('textarea');
    expect(pick(el('div', {}, other, answer), other)).toBe(answer);
    expect(pick(el('div', {}, other, el('button', { class: 'btn primary' })), other)).toBeNull();
  });
});

describe('Ctrl+Enter in the design questions', () => {
  const inQuestions = (focused: El, approve: El | null) =>
    questionsSubmitButton(focused as unknown as Element, approve as unknown as HTMLButtonElement | null) as unknown as El | null;

  it('presses Approve from an answer or the feedback, and Answer from AgentK is asking', () => {
    const approve = el('button', { class: 'btn small primary' });
    const note = el('textarea');
    const feedback = el('textarea');
    const askText = el('textarea');
    const answer = el('button', { class: 'btn small primary', 'data-submit': '' });
    el('div', { class: 'dq-scroll' }, el('div', { class: 'ask-form' }, askText, answer), el('ol', {}, note), feedback);
    expect(inQuestions(note, approve)).toBe(approve);
    expect(inQuestions(feedback, approve)).toBe(approve);
    expect(inQuestions(askText, approve)).toBe(answer);
    // An approval waiting for the turn's end shows Cancel approval instead: nothing to press.
    expect(inQuestions(feedback, null)).toBeNull();
  });
});
