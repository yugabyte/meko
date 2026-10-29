import { createHash } from 'node:crypto';
import type { MekoClaudeStatus } from '../shared/types.js';
import { resolveMeko, type Config } from './config.js';
import { errMsg, type Logger } from './log.js';
import type { TokenTry } from './meko.js';
import { claudeSettingsToken, mekoClaudeGaps, mekoUrlHost, type ClaudeMeko } from './mekosetup.js';

/**
 * Whether Meko's capture in Claude Code's sessions reaches the Meko AgentK uses (the capture part of
 * MekoStatus.claude; its `meko` MCP server is Meko's to report, `mekoagents.ts`). Claude Code keeps
 * its own copy of the server and token in its settings, where the Meko hooks read them, and it can
 * drift from AgentK's. Its token may differ from AgentK's; it must work on AgentK's server: accepted,
 * and seeing the project datapacks AgentK's own token sees. Nothing here changes Claude Code's setup:
 * that's only done when the user asks.
 */

/** How long a token's result and AgentK's own datapack list are reused. */
export const CLAUDE_MEKO_TTL_MS = 10 * 60 * 1000;

const PLACE = "the token in Claude Code's settings";

const EMPTY: MekoClaudeStatus = { gaps: [], capture: [], unchecked: [], checking: false };

export type TokenOutcome =
  | { kind: 'works' }
  | { kind: 'rejected' }
  | { kind: 'missing'; names: string[] }
  | { kind: 'unchecked'; error: string };

/** What a try of a token means, given the datapacks it must see. */
export function tokenOutcome(t: TokenTry, required: { id: string; name: string }[]): TokenOutcome {
  if (!t.ok) return t.rejected ? { kind: 'rejected' } : { kind: 'unchecked', error: t.error };
  const seen = new Set(t.datapacks);
  const names = required.filter((d) => !seen.has(d.id)).map((d) => d.name);
  return names.length ? { kind: 'missing', names } : { kind: 'works' };
}

function upper(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

/** The sentence for a token that doesn't work or couldn't be checked; null when it works. */
export function tokenOutcomeText(o: TokenOutcome, url: string): string | null {
  const host = mekoUrlHost(url);
  switch (o.kind) {
    case 'works':
      return null;
    case 'rejected':
      return `${upper(PLACE)} isn't accepted by ${host}.`;
    case 'missing': {
      const what = o.names.length === 1
        ? `the datapack ${o.names[0]}`
        : `${o.names.length} of AgentK's project datapacks (${o.names.slice(0, 2).join(', ')}${o.names.length > 2 ? ', …' : ''})`;
      return `${upper(PLACE)} can't see ${what} on ${host}.`;
    }
    case 'unchecked':
      return `Couldn't check ${PLACE} on ${host}: ${o.error.replace(/[.\s]+$/, '')}.`;
  }
}

/**
 * The installer's token check: Claude Code's token, unless it's AgentK's, must be accepted by
 * AgentK's server. It deliberately stops there, even on a reinstall whose projects already have
 * datapacks: whether it sees them is the server's check (MekoClaudeWatch), which runs once it
 * starts. One that couldn't be checked isn't a gap: the install goes on.
 */
export async function claudeTokenGaps(c: ClaudeMeko, url: string, apiKey: string, tryToken: (url: string, token: string) => Promise<TokenTry>): Promise<string[]> {
  const token = claudeSettingsToken(c, url);
  if (!token || token === apiKey) return [];
  const o = tokenOutcome(await tryToken(url, token), []);
  return o.kind === 'rejected' ? [tokenOutcomeText(o, url)!] : [];
}

function hash(s: string): string {
  return createHash('sha256').update(s).digest('hex');
}

export interface MekoClaudeWatchDeps {
  config: () => Config;
  /** Claude Code is available for sessions (turned on and installed): nothing to check without it. */
  available: () => boolean;
  /** What Meko's capture needs in Claude Code, as its files say now. */
  read: () => ClaudeMeko;
  /** Every open project's datapacks. */
  projectDatapacks: () => { id: string; name: string }[];
  /** The datapacks AgentK's own token sees. */
  ownDatapacks: () => Promise<string[]>;
  tryToken: (url: string, token: string) => Promise<TokenTry>;
  /** MekoClaudeWatch.status changed. */
  changed: () => void;
  log: Logger;
  now?: () => number;
}

/**
 * Keeps the capture part of MekoStatus.claude current. A refresh reads Claude Code's settings (cheap)
 * and tries its token, unless it's AgentK's own, on AgentK's server, reusing a token's result for
 * CLAUDE_MEKO_TTL_MS. Refreshes run one at a time; one asked for while another runs follows it.
 * Empty while Meko is off or Claude Code isn't available (Core refreshes it when that changes).
 */
export class MekoClaudeWatch {
  status: MekoClaudeStatus = EMPTY;
  private results = new Map<string, { at: number; outcome: TokenOutcome }>();
  private own: { key: string; at: number; ids: Set<string> } | null = null;
  private chain: Promise<void> = Promise.resolve();
  private next: Promise<void> | null = null;
  private forceNext = false;

  constructor(private o: MekoClaudeWatchDeps) {}

  /** Checks again; `force` skips the saved results (after Claude Code's setup ran). Never throws. */
  refresh(force = false): Promise<void> {
    if (force) this.forceNext = true;
    if (!this.next) {
      const next = this.chain.then(() => {
        this.next = null;
        const f = this.forceNext;
        this.forceNext = false;
        return this.run(f).catch((e) => {
          this.o.log.warn("checking Claude Code's Meko failed", errMsg(e));
          this.set({ ...this.status, checking: false });
        });
      });
      this.next = next;
      this.chain = next;
    }
    return this.next;
  }

  private set(s: MekoClaudeStatus): void {
    if (JSON.stringify(s) === JSON.stringify(this.status)) return;
    this.status = s;
    this.o.changed();
  }

  private async run(force: boolean): Promise<void> {
    const now = this.o.now?.() ?? Date.now();
    for (const [k, r] of this.results) if (now - r.at >= CLAUDE_MEKO_TTL_MS) this.results.delete(k);
    const { url, apiKey } = resolveMeko(this.o.config());
    if (!apiKey || !this.o.available()) {
      this.results.clear();
      this.own = null;
      this.set(EMPTY);
      return;
    }
    const c = this.o.read();
    const gaps = mekoClaudeGaps(c, url);
    const settingsToken = claudeSettingsToken(c, url);
    const token = settingsToken && settingsToken !== apiKey ? settingsToken : null;
    const required = this.o.projectDatapacks();
    const key = token ? [url, hash(apiKey), hash(token), required.map((d) => d.id).join(',')].join('\n') : '';
    let outcome = token && !force ? this.results.get(key)?.outcome : undefined;
    if (token && !outcome) {
      this.set({ ...this.status, checking: true });
      outcome = await this.check(url, apiKey, token, required, now, force);
      if (outcome.kind !== 'unchecked') this.results.set(key, { at: now, outcome });
    }
    const text = outcome ? tokenOutcomeText(outcome, url) : null;
    const unchecked = text && outcome?.kind === 'unchecked' ? [text] : [];
    if (text && outcome?.kind !== 'unchecked') gaps.push(text);
    this.set({ gaps, capture: gaps, unchecked, checking: false });
  }

  /** One token on AgentK's server, held to the project datapacks AgentK's own token sees. */
  private async check(
    url: string, apiKey: string, token: string, required: { id: string; name: string }[], now: number, force: boolean,
  ): Promise<TokenOutcome> {
    const ownKey = `${url}\n${hash(apiKey)}`;
    if (force || !this.own || this.own.key !== ownKey || now - this.own.at >= CLAUDE_MEKO_TTL_MS) {
      try {
        this.own = { key: ownKey, at: now, ids: new Set(await this.o.ownDatapacks()) };
      } catch (e) {
        this.own = null;
        return { kind: 'unchecked', error: `AgentK couldn't list its own datapacks (${errMsg(e)})` };
      }
    }
    const own = this.own.ids;
    // A datapack AgentK's own token can't see either (deleted in Meko, say) isn't held against Claude Code's.
    return tokenOutcome(await this.o.tryToken(url, token), required.filter((d) => own.has(d.id)));
  }
}
