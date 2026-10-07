import { describe, expect, it } from 'vitest';
import { filterItems, labelText, type LabelPart } from '../../src/web/palette.js';

const item = (id: string, label: LabelPart[], keywords = '') => ({ id, label, keywords });
const items = [
  item('move', ['Move ', { session: 'Settings page' }, ' to another stage'], 'change stage'),
  item('s1', [{ session: 'Fix login' }], 'agent-k-local Fix login ak/fix-login'),
  item('home', [{ project: 'agent-k-local' }, ': project home']),
  item('settings', ['Settings'], 'settings password'),
];
const ids = (q: string) => filterItems(items, q).map((it) => it.id);

describe('command palette labels', () => {
  it('reads a label with names in it as plain text', () => {
    expect(labelText(items[0].label)).toBe('Move Settings page to another stage');
    expect(labelText(items[2].label)).toBe('agent-k-local: project home');
    expect(labelText(['Usage'])).toBe('Usage');
  });

  it('finds items by the names in their labels and by their keywords', () => {
    expect(ids('')).toEqual(['move', 's1', 'home', 'settings']);
    expect(ids('settings')).toEqual(['move', 'settings']);
    expect(ids('move settings page')).toEqual(['move']);
    expect(ids('page settings move')).toEqual(['move']);
    expect(ids('AGENT-K-LOCAL home')).toEqual(['home']);
    expect(ids('fix-login')).toEqual(['s1']);
    expect(ids('nothing like this')).toEqual([]);
  });
});
