import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import type { HarnessStatus, Project } from '../../src/shared/types.js';
import { NewSessionDialog } from '../../src/web/components/NewSessionDialog.js';

const project = { id: 'p', name: 'Demo', repoPath: '/code/demo', defaultBranch: 'main', settings: { branchPrefix: 'ak/' } } as unknown as Project;
const status = (id: HarnessStatus['id'], o: Partial<HarnessStatus> = {}): HarnessStatus => ({
  id, enabled: true, version: '1.0.0', minVersion: null, available: true, checked: true, isDefault: id === 'claude', updating: false, ...o,
});

/** The dialog with Advanced open (a retry on another repo opens it). */
const render = (harnesses: HarnessStatus[], harness?: HarnessStatus['id']) =>
  renderToStaticMarkup(createElement(NewSessionDialog, {
    project, ghLogin: null, harnesses, initial: { name: 'x', repoPath: '/code/other', ...(harness ? { harness } : {}) },
    onClose: () => undefined, onCreated: () => undefined,
  }));

/** The Agent choice's buttons in the dialog. */
function agents(harnesses: HarnessStatus[], harness?: HarnessStatus['id']) {
  const html = render(harnesses, harness);
  return [...html.matchAll(/<button[^>]*id="ns-agent-(\w+)"[^>]*aria-checked="(true|false)"/g)].map(([, id, on]) => `${id}${on === 'true' ? '*' : ''}`);
}

describe("the New session dialog's Agent choice", () => {
  it.each([
    ['is left out while only Claude Code is available', [status('claude'), status('kiro', { available: false })], undefined, []],
    ['starts on the default agent', [status('claude', { isDefault: false }), status('kiro', { isDefault: true })], undefined, ['claude', 'kiro*']],
    ['keeps the agent a failed start asked for', [status('claude'), status('kiro')], 'kiro' as const, ['claude', 'kiro*']],
  ])('%s', (_, harnesses, harness, shown) => {
    expect(agents(harnesses, harness)).toEqual(shown);
  });

  it.each([
    ['says so once every agent has answered', [status('claude', { available: false, version: null }), status('kiro', { enabled: false, available: false })], true],
    ['waits for an agent still being asked', [status('claude', { available: false, version: null, checked: false })], false],
  ])('with no agent available, %s', (_, harnesses, said) => {
    const html = render(harnesses);
    expect(html.includes('No coding agent is available')).toBe(said);
  });
});
