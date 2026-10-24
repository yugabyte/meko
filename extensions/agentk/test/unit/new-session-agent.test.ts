import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import type { HarnessStatus, Project, Settings } from '../../src/shared/types.js';
import { NewSessionDialog } from '../../src/web/components/NewSessionDialog.js';
import { AgentSettings, HarnessBar } from '../../src/web/components/SettingsDialog.js';
import { defaultAgentChoice } from '../../src/web/util.js';

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

/**
 * The Agent choice's buttons in the dialog: each one's id, `*` when it's picked, and ` (no logo)`
 * when it doesn't show its agent's logo.
 */
function agents(harnesses: HarnessStatus[], harness?: HarnessStatus['id']) {
  const html = render(harnesses, harness);
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

  it.each([
    ['says so once every agent has answered', [status('claude', { available: false, version: null }), status('kiro', { enabled: false, available: false })], true],
    ['waits for an agent still being asked', [status('claude', { available: false, version: null, checked: false })], false],
  ])('with no agent available, %s', (_, harnesses, said) => {
    const html = render(harnesses);
    expect(html.includes('No coding agent is available')).toBe(said);
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

describe('Settings › Harness', () => {
  it('lists every agent in its bar, an agent that is off too', () => {
    const html = renderToStaticMarkup(createElement(HarnessBar, { value: 'kiro', onChange: () => undefined }));
    const tabs = [...html.matchAll(/id="set-harness-tab-(\w+)"[^>]*aria-selected="(true|false)"/g)].map(([, id, on]) => `${id}${on === 'true' ? '*' : ''}`);
    expect(tabs).toEqual(['claude', 'opencode', 'kiro*']);
  });

  const settings = { claudeEnabled: true, opencodeEnabled: true, kiroEnabled: true, kiroTrustTools: false, defaultHarness: 'claude' } as Settings;
  /** The ids of the controls (and the update outcome) an agent's panel shows, then `⚠️` when its note starts with one. */
  const controls = (id: HarnessStatus['id'], s: Partial<Settings>, st: Partial<HarnessStatus>, outcome: { ok: boolean; text: string } | null = null) => {
    const html = renderToStaticMarkup(createElement(AgentSettings, {
      id, s: { ...settings, ...s }, status: status(id, st), checking: false, updating: false, outcome,
      onEnable: () => undefined, onChange: () => undefined, onCheck: () => undefined, onUpdate: () => undefined,
    }));
    const ids = [...html.matchAll(/<(?:input|button|span)[^>]*id="(set-[\w-]+)"/g)].map(([, x]) => x);
    return html.includes('<span class="hint">⚠️ ') ? [...ids, '⚠️'] : ids;
  };

  it.each([
    ['Claude Code, available', 'claude' as const, {}, {}, ['set-claude', 'set-claude-check', 'set-claude-update'], null],
    ['Claude Code, after an update the banners leave out', 'claude' as const, {}, {}, ['set-claude', 'set-claude-check', 'set-claude-update', 'set-claude-outcome'], { ok: false, text: 'x' }],
    ['OpenCode, available', 'opencode' as const, {}, {}, ['set-opencode', 'set-opencode-check', 'set-opencode-update'], null],
    ['Kiro, available', 'kiro' as const, {}, {}, ['set-kiro', 'set-kiro-check', 'set-kiro-update', 'set-kiro-trust'], null],
    ['Kiro, on but not installed', 'kiro' as const, {}, { available: false, version: null }, ['set-kiro', 'set-kiro-check', 'set-kiro-trust', '⚠️'], null],
    ['Kiro, off', 'kiro' as const, { kiroEnabled: false }, { enabled: false, available: false }, ['set-kiro'], null],
  ])("%s: the panel shows only that agent's settings, and ⚠️ when it's missing", (_, id, s, st, shown, outcome) => {
    expect(controls(id, s, st, outcome)).toEqual(shown);
  });
});
