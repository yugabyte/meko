import { describe, expect, it } from 'vitest';
import { submitButton } from '../../src/web/components/common.js';
import { isSubmitKey } from '../../src/web/util.js';

/** Just enough of an element for submitButton's selectors: tag, classes, attributes and `.a b` descendants. */
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
  private is(simple: string): boolean {
    const m = /^(\w*)((?:\.[\w-]+)*)((?:\[[\w-]+\])*)(?::not\(\[(\w+)="(\w+)"\]\))?$/.exec(simple);
    if (!m) throw new Error(`unsupported selector ${simple}`);
    const [, tag, classes, attrs, notAttr, notValue] = m;
    const cls = (this.attrs.class ?? '').split(' ');
    return (
      (!tag || tag === this.tag) &&
      classes.split('.').filter(Boolean).every((c) => cls.includes(c)) &&
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
    [{ key: 'Enter', ctrlKey: true, metaKey: false }, true],
    [{ key: 'Enter', ctrlKey: false, metaKey: true }, true],
    [{ key: 'Enter', ctrlKey: false, metaKey: false }, false],
    [{ key: 'a', ctrlKey: true, metaKey: false }, false],
  ])('takes Ctrl or ⌘ plus Enter only: %o', (e, sends) => {
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

    // AgentK is asking: no form or footer, an Answer marked as the dialog's.
    const answer = el('button', { class: 'btn small primary', 'data-submit': '' });
    const other = el('textarea');
    expect(pick(el('div', {}, other, answer), other)).toBe(answer);
    expect(pick(el('div', {}, other, el('button', { class: 'btn primary' })), other)).toBeNull();
  });
});
