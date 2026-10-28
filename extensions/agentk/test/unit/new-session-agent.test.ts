import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { HARNESS_CAPS } from '../../src/shared/harnesses.js';
import type { HarnessStatus, MekoStatus, Project, Settings } from '../../src/shared/types.js';
import { NewSessionDialog } from '../../src/web/components/NewSessionDialog.js';
import { AgentSettings, HarnessBar, MekoLinks } from '../../src/web/components/SettingsDialog.js';
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

describe("the New session dialog's Learn the project", () => {
  const dedicated = { ...project, datapacks: [{ id: 'dp', name: 'adlc/proj/demo/x', role: 'dedicated' }] } as unknown as Project;
  const learnHtml = (p: Project, mekoOn: boolean, learnFirst: boolean) =>
    renderToStaticMarkup(createElement(NewSessionDialog, {
      project: p, ghLogin: null, harnesses: [status('claude')], mekoOn, learnFirst, onClose: () => undefined, onCreated: () => undefined,
    }));
  it.each([
    ['is picked, with its fields and the empty-datapack line, for a project just opened on an empty datapack', dedicated, true, true, { pill: true, picked: true, fields: true }],
    ['is offered but not picked otherwise', dedicated, true, false, { pill: true, picked: false, fields: false }],
    ["isn't offered without a dedicated datapack", { ...project, datapacks: [] } as unknown as Project, true, true, { pill: false, picked: false, fields: false }],
    ["isn't offered with Meko off", dedicated, false, true, { pill: false, picked: false, fields: false }],
  ])('%s', (_, p, mekoOn, learnFirst, want) => {
    const html = learnHtml(p, mekoOn, learnFirst);
    expect({
      pill: html.includes('id="ns-start-learn"'),
      picked: /id="ns-start-learn"[^>]*aria-checked="true"/.test(html),
      fields: html.includes('id="ns-learn-about"') && html.includes('id="ns-learn-offer"') && !html.includes('id="ns-prompt"'),
    }).toEqual(want);
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

  const settings = { claudeEnabled: true, opencodeEnabled: true, opencodeDisableTracking: false, kiroEnabled: true, kiroTrustTools: false, defaultHarness: 'claude' } as Settings;
  /**
   * The ids of the controls (and the update outcome) an agent's panel shows, then `⚠️` when its note
   * starts with one, and the agent's upgrade command when its note names it.
   */
  const controls = (
    id: HarnessStatus['id'], s: Partial<Settings>, st: Partial<HarnessStatus>, outcome: { ok: boolean; text: string } | null = null, meko?: MekoStatus,
  ) => {
    const html = renderToStaticMarkup(createElement(AgentSettings, {
      id, s: { ...settings, ...s }, status: status(id, st), checking: false, updating: false, outcome, meko,
      onEnable: () => undefined, onChange: () => undefined, onCheck: () => undefined, onUpdate: () => undefined,
    }));
    const ids = [...html.matchAll(/<(?:input|button|span)[^>]*id="(set-[\w-]+)"/g)].map(([, x]) => x);
    const upgrade = HARNESS_CAPS[id].upgrade;
    return [...ids, ...(html.includes('<span class="hint">⚠️ ') ? ['⚠️'] : []), ...(upgrade && html.includes(upgrade) ? [upgrade] : [])];
  };

  it.each([
    ['Claude Code, available', 'claude' as const, {}, {}, ['set-claude', 'set-claude-check', 'set-claude-update'], null],
    ['Claude Code, after an update the banners leave out', 'claude' as const, {}, {}, ['set-claude', 'set-claude-check', 'set-claude-update', 'set-claude-outcome'], { ok: false, text: 'x' }],
    ['OpenCode, available', 'opencode' as const, {}, {}, ['set-opencode', 'set-opencode-check', 'set-opencode-update', 'set-opencode-tracking'], null],
    ['OpenCode, too old', 'opencode' as const, {}, { version: '1.19.4', minVersion: '2.0.0', available: false }, ['set-opencode', 'set-opencode-check', 'set-opencode-tracking', 'opencode upgrade --major'], null],
    ['Kiro, available', 'kiro' as const, {}, {}, ['set-kiro', 'set-kiro-check', 'set-kiro-update', 'set-kiro-trust'], null],
    ['Kiro, on but not installed', 'kiro' as const, {}, { available: false, version: null }, ['set-kiro', 'set-kiro-check', 'set-kiro-trust', '⚠️'], null],
    ['Kiro, off', 'kiro' as const, { kiroEnabled: false }, { enabled: false, available: false }, ['set-kiro'], null],
  ])("%s: the panel shows only that agent's settings, ⚠️ when it's missing, and how to upgrade one too old", (_, id, s, st, shown, outcome) => {
    expect(controls(id, s, st, outcome)).toEqual(shown);
  });

  const meko = (o: Partial<MekoStatus> = {}): MekoStatus => ({
    configured: true, url: 'https://mcp.mekodata.ai/mcp', source: 'meko', webUrl: null, local: 'running',
    claude: { gaps: [], unchecked: [], checking: false }, kiro: { gaps: [] }, opencode: { own: false }, ...o,
  });
  it("keeps an agent's explanations behind a closed ⓘ, outside its checkbox's label, opening under the row", () => {
    const html = renderToStaticMarkup(createElement(AgentSettings, {
      id: 'kiro', s: settings, status: status('kiro', {}), checking: false, updating: false,
      onEnable: () => undefined, onChange: () => undefined, onCheck: () => undefined, onUpdate: () => undefined,
    }));
    expect(html).toMatch(/<\/label><button type="button" class="icon-btn info-btn" aria-expanded="false" aria-controls="set-kiro-trust-info"/);
    expect(html).toMatch(/<\/button><\/div><p id="set-kiro-trust-info" class="info-panel" hidden="">/);
  });

  it.each([
    ['Claude Code on another Meko', 'claude' as const, meko({ claude: { gaps: ['x'], unchecked: [], checking: false } }),
      ['set-claude', 'set-claude-check', 'set-claude-update', 'set-claude-meko', 'set-claude-meko-setup']],
    ['Claude Code on this Meko', 'claude' as const, meko(), ['set-claude', 'set-claude-check', 'set-claude-update', 'set-claude-meko']],
    ['OpenCode with its own', 'opencode' as const, meko({ opencode: { own: true } }),
      ['set-opencode', 'set-opencode-check', 'set-opencode-update', 'set-opencode-meko', 'set-opencode-tracking']],
    ['Kiro on another Meko', 'kiro' as const, meko({ kiro: { gaps: ['x'] } }),
      ['set-kiro', 'set-kiro-check', 'set-kiro-update', 'set-kiro-meko', 'set-kiro-meko-setup', 'set-kiro-trust']],
    ['no Meko', 'claude' as const, meko({ configured: false, url: null, source: null }), ['set-claude', 'set-claude-check', 'set-claude-update']],
  ])("%s: the agent's panel says whether its sessions reach AgentK's Meko, with Meko's setup when they don't", (_, id, m, shown) => {
    expect(controls(id, {}, {}, null, m)).toEqual(shown);
  });
});

describe('Settings › General', () => {
  it("links to Meko's Meko Cloud and account pages", () => {
    const html = renderToStaticMarkup(createElement(MekoLinks, {
      meko: { configured: true, url: 'http://127.0.0.1:7778/mcp', source: 'local', webUrl: null, local: 'running', claude: { gaps: [], unchecked: [], checking: false }, kiro: null, opencode: null },
      auth: { access: 'local', signedIn: true },
    }));
    expect([...html.matchAll(/<a[^>]*id="(set-meko-[\w-]+)"[^>]*href="([^"]+)"/g)].map(([, id, href]) => `${id} ${href}`))
      .toEqual(['set-meko-cloud /settings/meko-cloud', 'set-meko-account /account']);
    // The ⓘ's text opens under the setting, after its status line.
    expect(html).toMatch(/<span class="hint">AgentK uses the Meko on this computer\.<\/span><p id="set-meko-info" class="info-panel" hidden="">Meko opens AgentK on this computer without a sign-in\./);
  });
});
