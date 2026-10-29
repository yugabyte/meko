import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { HARNESS_CAPS } from '../../src/shared/harnesses.js';
import type { CreateSessionRequest, HarnessMekoState, HarnessStatus, MekoStatus, Project, Settings } from '../../src/shared/types.js';
import { NewSessionDialog } from '../../src/web/components/NewSessionDialog.js';
import { AgentSettings, HarnessBar, MekoLinks } from '../../src/web/components/SettingsDialog.js';
import { connectToMeko, setUpWithMeko } from '../../src/web/mekoagents.js';
import { defaultAgentChoice } from '../../src/web/util.js';

const project = { id: 'p', name: 'Demo', repoPath: '/code/demo', defaultBranch: 'main', settings: { branchPrefix: 'ak/' } } as unknown as Project;
const status = (id: HarnessStatus['id'], o: Partial<HarnessStatus> = {}): HarnessStatus => ({
  id, enabled: true, version: '1.0.0', minVersion: null, available: true, checked: true, isDefault: id === 'claude', updating: false, meko: null, ...o,
});
/** Ready for sessions but for Meko, which reports it in `state`. */
const offMeko = (state: HarnessMekoState, url: string | null = null): Partial<HarnessStatus> => ({ available: false, meko: { state, url } });

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

  /** Where the dialog names an agent Meko hasn't connected: `top`, `advanced` (under Agent), or `none`. */
  const notConnectedAt = (html: string) => {
    const at = html.indexOf('id="ns-agent-meko"');
    return at < 0 ? 'none' : at < html.indexOf('id="ns-advanced"') ? 'top' : 'advanced';
  };
  it.each([
    ['under Agent, for one that isn\'t the default', [status('claude'), status('opencode'), status('kiro', offMeko('none'))], 'advanced', false],
    ['at the top, for the default, which the dialog would otherwise start on', [status('claude', { ...offMeko('stale') }), status('kiro')], 'top', false],
    ['with the no-agent line, when it was the only one', [status('claude', offMeko('elsewhere', 'https://meko.example.com/mcp'))], 'none', true],
  ])("names an agent Meko hasn't connected, with the fix, %s", (_, harnesses, at, none) => {
    const html = render(harnesses);
    expect(notConnectedAt(html)).toBe(at);
    expect(html.includes('No coding agent is available')).toBe(none);
    const id = harnesses.find((h) => h.meko)!.id;
    expect(html).toContain(`${HARNESS_CAPS[id].label} isn&#x27;t connected to Meko: connect it in Settings › Harness, or run meko agent connect ${id}.`);
  });

  it("offers an agent Meko didn't find, saying nothing about it", () => {
    const html = render([status('claude'), status('kiro', { meko: { state: 'missing', url: null } })]);
    expect(notConnectedAt(html)).toBe('none');
    expect(agents([status('claude'), status('kiro', { meko: { state: 'missing', url: null } })])).toEqual(['claude*', 'kiro']);
  });
});

describe("the New session dialog's Learn the project", () => {
  const dedicated = { ...project, datapacks: [{ id: 'dp', name: 'adlc/proj/demo/x', role: 'dedicated' }] } as unknown as Project;
  const learnHtml = (p: Project, mekoOn: boolean, learnFirst: boolean, initial?: CreateSessionRequest) =>
    renderToStaticMarkup(createElement(NewSessionDialog, {
      project: p, ghLogin: null, harnesses: [status('claude')], mekoOn, learnFirst, initial, onClose: () => undefined, onCreated: () => undefined,
    }));
  const shown = (html: string) => ({
    pill: html.includes('id="ns-start-learn"'),
    picked: /id="ns-start-learn"[^>]*aria-checked="true"/.test(html),
    fields: html.includes('id="ns-learn-about"') && !html.includes('id="ns-prompt"'),
    offer: html.includes('id="ns-learn-offer"'),
  });
  const retryLearn: CreateSessionRequest = { name: 'Learn the project', learn: { about: 'The billing service.', where: 'services/billing/', months: 12 } };
  const retryDesign: CreateSessionRequest = { name: 'x', start: 'design', prompt: 'Fix it' };
  it.each([
    ['is picked, with its fields and the empty-datapack line, for a project just opened on an empty datapack', dedicated, true, true, undefined, { pill: true, picked: true, fields: true, offer: true }],
    ["isn't offered in a plain New session, even with Meko on and a dedicated datapack", dedicated, true, false, undefined, { pill: false, picked: false, fields: false, offer: false }],
    ['is offered and picked again to retry a failed learn start', dedicated, true, false, retryLearn, { pill: true, picked: true, fields: true, offer: false }],
    ["isn't offered to retry a failed start at Design", dedicated, true, false, retryDesign, { pill: false, picked: false, fields: false, offer: false }],
    ["isn't offered without a dedicated datapack", { ...project, datapacks: [] } as unknown as Project, true, true, undefined, { pill: false, picked: false, fields: false, offer: false }],
    ["isn't offered with Meko off", dedicated, false, true, undefined, { pill: false, picked: false, fields: false, offer: false }],
  ])('%s', (_, p, mekoOn, learnFirst, initial, want) => {
    const html = learnHtml(p, mekoOn, learnFirst, initial);
    expect(shown(html)).toEqual(want);
    // A retry keeps what it asked for.
    if (initial?.learn) expect([html.includes('The billing service.'), html.includes('services/billing/')]).toEqual([true, true]);
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
    configured: true, url: 'https://mcp.mekodata.ai/mcp', source: 'meko', webUrl: null, sharing: true, local: 'running', agentList: 'read',
    claude: { gaps: [], capture: [], unchecked: [], checking: false }, kiro: { gaps: [], capture: [] }, opencode: { own: false }, ...o,
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
    ['Claude Code on another Meko', 'claude' as const, meko({ claude: { gaps: ['x'], capture: ['x'], unchecked: [], checking: false } }),
      ['set-claude', 'set-claude-check', 'set-claude-update', 'set-claude-meko', 'set-claude-meko-setup']],
    ['Claude Code on this Meko', 'claude' as const, meko(), ['set-claude', 'set-claude-check', 'set-claude-update', 'set-claude-meko']],
    ['OpenCode with its own', 'opencode' as const, meko({ opencode: { own: true } }),
      ['set-opencode', 'set-opencode-check', 'set-opencode-update', 'set-opencode-meko', 'set-opencode-tracking']],
    ['Kiro on another Meko', 'kiro' as const, meko({ kiro: { gaps: ['x'], capture: ['x'] } }),
      ['set-kiro', 'set-kiro-check', 'set-kiro-update', 'set-kiro-meko', 'set-kiro-meko-setup', 'set-kiro-trust']],
    ['no Meko', 'claude' as const, meko({ configured: false, url: null, source: null }), ['set-claude', 'set-claude-check', 'set-claude-update']],
  ])("%s: the agent's panel says whether its sessions reach AgentK's Meko, with Meko's setup when they don't", (_, id, m, shown) => {
    expect(controls(id, {}, {}, null, m)).toEqual(shown);
  });

  /** The agent's Meko row: its tag, its hint, and its button (`connect`, `setup` or none). */
  const mekoRow = (id: HarnessStatus['id'], st: Partial<HarnessStatus>, m: MekoStatus) => {
    const html = renderToStaticMarkup(createElement(AgentSettings, {
      id, s: settings, status: status(id, st), checking: false, updating: false, meko: m,
      onEnable: () => undefined, onChange: () => undefined, onCheck: () => undefined, onUpdate: () => undefined,
    }));
    const row = html.slice(html.indexOf('>Meko'));
    return {
      tag: /id="set-\w+-meko"[^>]*>([^<]*)</.exec(row)?.[1] ?? null,
      hint: /<span class="hint">([^<]*)</.exec(row)?.[1]?.replace(/&#x27;|’/g, "'") ?? null,
      button: row.includes('-meko-connect"') ? 'connect' : row.includes('-meko-setup"') ? 'setup' : null,
    };
  };
  it.each([
    ['connected', 'kiro' as const, { meko: { state: 'connected' as const, url: 'https://mcp.mekodata.ai/mcp' } }, meko(),
      { tag: 'connected', hint: 'Sessions use https://mcp.mekodata.ai/mcp.', button: null }],
    ['needs reconnecting', 'kiro' as const, offMeko('stale'), meko(), { tag: 'needs reconnecting', hint: null, button: 'connect' }],
    ['connected elsewhere', 'opencode' as const, offMeko('elsewhere', 'https://meko.example.com/mcp'), meko(),
      { tag: 'connected elsewhere', hint: 'Connected elsewhere: https://meko.example.com/mcp.', button: 'connect' }],
    ['not connected', 'claude' as const, offMeko('none'), meko({ claude: { gaps: ['Claude Code has no meko MCP server.'], capture: [], unchecked: [], checking: false } }),
      { tag: 'not connected', hint: null, button: 'connect' }],
    ["not connected, and capture missing: one Set up does both", 'claude' as const, offMeko('none'),
      meko({ claude: { gaps: ['Claude Code has no meko MCP server.', 'No plugin.'], capture: ['No plugin.'], unchecked: [], checking: false } }),
      { tag: 'not connected', hint: 'No plugin.', button: 'setup' }],
    ["Meko didn't find it (still offered)", 'kiro' as const, { meko: { state: 'missing' as const, url: null } }, meko(), { tag: 'not found', hint: "Meko didn't find Kiro on this computer.", button: null }],
    ['a Meko too old to list agents', 'kiro' as const, {}, meko({ agentList: 'old' }),
      { tag: 'matches', hint: 'Update Meko (meko update) to manage agent connections.', button: null }],
  ])("%s: the agent's Meko row says so", (_, id, st, m, want) => {
    expect(mekoRow(id, st, m)).toEqual(want);
  });
});

describe('Settings › General', () => {
  it("links to Meko's Meko Cloud and account pages", () => {
    const html = renderToStaticMarkup(createElement(MekoLinks, {
      meko: { configured: true, url: 'http://127.0.0.1:7778/mcp', source: 'local', webUrl: null, sharing: false, local: 'running', claude: { gaps: [], capture: [], unchecked: [], checking: false }, kiro: null, opencode: null, agentList: 'unread' },
      auth: { access: 'local', signedIn: true },
    }));
    expect([...html.matchAll(/<a[^>]*id="(set-meko-[\w-]+)"[^>]*href="([^"]+)"/g)].map(([, id, href]) => `${id} ${href}`))
      .toEqual(['set-meko-cloud /settings/meko-cloud', 'set-meko-account /account']);
    // The ⓘ's text opens under the setting, after its status line.
    expect(html).toMatch(/<span class="hint">AgentK uses the Meko on this computer\.<\/span><p id="set-meko-info" class="info-panel" hidden="">Meko opens AgentK on this computer without a sign-in\./);
  });
});

describe('Connect to Meko, from the page', () => {
  /** A fetch that answers each path with its own reply, recording each call's method, path and credentials. */
  const stubFetch = (replies: Record<string, { status: number; body: unknown }>) => {
    const calls: string[] = [];
    vi.stubGlobal('fetch', async (path: string, init: RequestInit) => {
      calls.push(`${init.method} ${path} ${init.credentials}`);
      const r = replies[path] ?? { status: 404, body: {} };
      return new Response(JSON.stringify(r.body), { status: r.status, headers: { 'Content-Type': 'application/json' } });
    });
    return calls;
  };
  afterEach(() => vi.unstubAllGlobals());

  it("posts Meko's own route at the origin root with the page's sign-in, then has AgentK read Meko's list again", async () => {
    const harnesses = [status('kiro', { meko: { state: 'connected', url: 'http://127.0.0.1:7778/mcp' } })];
    const calls = stubFetch({
      '/api/v1/agents/kiro/connect': { status: 200, body: { data: { agent: {}, notes: ['It replaced the meko server at https://meko.example.com/mcp'] } } },
      '/api/harnesses/refresh': { status: 200, body: harnesses },
    });
    expect(await connectToMeko('kiro')).toEqual({ notes: ['It replaced the meko server at https://meko.example.com/mcp'], harnesses });
    expect(calls).toEqual(['POST /api/v1/agents/kiro/connect same-origin', 'POST /api/harnesses/refresh same-origin']);
  });

  it.each([
    ["Meko's refusal", { status: 409, body: { error: { code: 'AGENT_NOT_INSTALLED', message: "Kiro CLI isn't installed" } } }, "Kiro CLI isn't installed"],
    ['a Meko too old to connect agents', { status: 404, body: {} }, 'Update Meko (meko update) to manage agent connections.'],
  ])("says %s, and doesn't refresh", async (_, reply, message) => {
    const calls = stubFetch({ '/api/v1/agents/kiro/connect': reply });
    await expect(connectToMeko('kiro')).rejects.toThrow(message);
    expect(calls).toEqual(['POST /api/v1/agents/kiro/connect same-origin']);
  });

  it.each([
    ['connects it first when it is not connected', { state: 'none' as const, url: null }, ['POST /api/v1/agents/claude/connect same-origin', 'POST /api/settings/meko/claude-setup same-origin']],
    ['goes straight to the setup when it is', { state: 'connected' as const, url: 'http://127.0.0.1:7778/mcp' }, ['POST /api/settings/meko/claude-setup same-origin']],
    ['goes straight to the setup without a list to go by', null, ['POST /api/settings/meko/claude-setup same-origin']],
  ])('Set up Claude Code with this Meko %s', async (_, meko, want) => {
    const calls = stubFetch({
      '/api/v1/agents/claude/connect': { status: 200, body: { data: { agent: {}, notes: [] } } },
      '/api/settings/meko/claude-setup': { status: 200, body: {} },
    });
    await setUpWithMeko('claude', meko);
    expect(calls).toEqual(want);
  });
});
