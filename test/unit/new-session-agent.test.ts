import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import type { HarnessStatus, Project } from '../../src/shared/types.js';
import { NewSessionDialog } from '../../src/web/components/NewSessionDialog.js';
import { defaultAgentChoice } from '../../src/web/util.js';

const project = { id: 'p', name: 'Demo', repoPath: '/code/demo', defaultBranch: 'main', settings: { branchPrefix: 'ak/' } } as unknown as Project;
const status = (id: HarnessStatus['id'], o: Partial<HarnessStatus> = {}): HarnessStatus => ({
  id, enabled: true, version: '1.0.0', minVersion: null, available: true, isDefault: id === 'claude', updating: false, ...o,
});

/**
 * The dialog with Advanced open (a retry on another repo opens it), and the Agent choice's buttons
 * in it: each one's id, `*` when it's picked, and ` (no logo)` when it doesn't show its agent's logo.
 */
function agents(harnesses: HarnessStatus[], harness?: HarnessStatus['id']) {
  const html = renderToStaticMarkup(createElement(NewSessionDialog, {
    project, ghLogin: null, harnesses, initial: { name: 'x', repoPath: '/code/other', ...(harness ? { harness } : {}) },
    onClose: () => undefined, onCreated: () => undefined,
  }));
  return [...html.matchAll(/<button[^>]*id="ns-agent-(\w+)"[^>]*aria-checked="(true|false)"[^>]*>(.*?)<\/button>/g)].map(
    ([, id, on, inner]) => `${id}${on === 'true' ? '*' : ''}${inner.includes(`src="/harness-${id}.svg"`) ? '' : ' (no logo)'}`,
  );
}

describe("the New session dialog's Agent choice", () => {
  it.each([
    ['is left out while only Claude Code is available', [status('claude'), status('kiro', { available: false })], undefined, []],
    ['starts on the default agent', [status('claude', { isDefault: false }), status('kiro', { isDefault: true })], undefined, ['claude', 'kiro*']],
    ['keeps the agent a failed start asked for', [status('claude'), status('kiro')], 'kiro' as const, ['claude', 'kiro*']],
  ])('%s', (_, harnesses, harness, shown) => {
    expect(agents(harnesses, harness)).toEqual(shown);
  });
});

describe("Settings' Default agent", () => {
  it.each([
    ['is left out while Kiro is off', [status('claude'), status('kiro', { enabled: false, available: false })], 'claude' as const, null],
    ['is left out, not offered disabled, while Kiro is too old', [status('claude'), status('kiro', { available: false })], 'kiro' as const, null],
    ['offers the available agents with the stored default picked', [status('claude'), status('kiro')], 'kiro' as const, { agents: ['claude', 'kiro'], value: 'kiro' }],
  ])('%s', (_, harnesses, stored, shown) => {
    expect(defaultAgentChoice(harnesses, stored)).toEqual(shown);
  });
});
