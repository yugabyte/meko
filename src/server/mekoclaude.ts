import { createHash } from 'node:crypto';
import type { MekoClaudeStatus } from '../shared/types.js';
import { resolveMeko, type Config } from './config.js';
import { errMsg, type Logger } from './log.js';
import type { TokenTry } from './meko.js';
import { claudeMekoTokens, mekoClaudeGaps, mekoUrlHost, type ClaudeMeko, type ClaudeTokenPlace } from './mekosetup.js';

/**
 * Whether Claude Code's sessions reach the Meko AgentK uses (MekoStatus.claude). Claude Code keeps
 * its own copy of the server and token in two places, its settings (the Meko hooks) and its `meko`
 * MCP server (the tools), and either can drift from AgentK's. Its tokens may differ from AgentK's;
 * each must work on AgentK's server: accepted, and seeing the project datapacks AgentK's own token
 * sees. Nothing here changes Claude Code's setup: that's only done when the user asks.
 */

/** How long a token's result, AgentK's own datapack list and the `claude mcp get` answer are reused. */
export const CLAUDE_MEKO_TTL_MS = 10 * 60 * 1000;

const PLACE: Record<ClaudeTokenPlace, string> = {
  settings: "the token in Claude Code's settings",
  server: "the token on Claude Code's meko MCP server",
};

const EMPTY: MekoClaudeStatus = { gaps: [], unchecked: [], checking: false };

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
export function tokenOutcomeText(place: ClaudeTokenPlace, o: TokenOutcome, url: string): string | null {
  const host = mekoUrlHost(url);
  switch (o.kind) {
    case 'works':
      return null;
    case 'rejected':
      return `${upper(PLACE[place])} isn't accepted by ${host}.`;
    case 'missing': {
      const what = o.names.length === 1
        ? `the datapack ${o.names[0]}`
        : `${o.names.length} of AgentK's project datapacks (${o.names.slice(0, 2).join(', ')}${o.names.length > 2 ? ', …' : ''})`;
      return `${upper(PLACE[place])} can't see ${what} on ${host}.`;
    }
    case 'unchecked':
      return `Couldn't check ${PLACE[place]} on ${host}: ${o.error.replace(/[.\s]+$/, '')}.`;
  }
}

/**
 * The installer's token check: each of Claude Code's tokens that isn't AgentK's must be accepted by
 * AgentK's server. It deliberately stops there, even on a reinstall whose projects already have
 * datapacks: whether a token sees them is the server's check (MekoClaudeWatch), which runs once it
 * starts. One that couldn't be checked isn't a gap: the install goes on.
 */
export async function claudeTokenGaps(c: ClaudeMeko, url: string, apiKey: string, tryToken: (url: string, token: string) => Promise<TokenTry>): Promise<string[]> {
  const gaps: string[] = [];
  const tried = new Map<string, TokenOutcome>();
  for (const t of claudeMekoTokens(c, url)) {
    if (t.token === apiKey) continue;
    if (!tried.has(t.token)) tried.set(t.token, tokenOutcome(await tryToken(url, t.token), []));
    const o = tried.get(t.token)!;
    if (o.kind === 'rejected') gaps.push(tokenOutcomeText(t.place, o, url)!);
  }
  return gaps;
}

function hash(s: string): string {
  return createHash('sha256').update(s).digest('hex');
}

export interface MekoClaudeWatchDeps {
  config: () => Config;
  /** Claude Code's Meko as its files say now. */
  read: () => ClaudeMeko;
  /**
   * Claude Code has a `meko` MCP server some other way; asked only when ~/.claude.json has no
   * user-scope one. Null when Claude Code couldn't be asked: that's unchecked, never a gap.
   */
  mcpRegistered: () => Promise<boolean | null>;
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
 * Keeps MekoStatus.claude current. A refresh reads Claude Code's files (cheap) and tries each of
 * its tokens that isn't AgentK's own on AgentK's server, reusing a token's result for
 * CLAUDE_MEKO_TTL_MS. Refreshes run one at a time; one asked for while another runs follows it.
 */
export class MekoClaudeWatch {
  status: MekoClaudeStatus = EMPTY;
  private results = new Map<string, { at: number; outcome: TokenOutcome }>();
  private own: { key: string; at: number; ids: Set<string> } | null = null;
  private registered: { at: number; value: boolean } | null = null;
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
    if (!apiKey) {
      this.results.clear();
      this.own = null;
      this.set(EMPTY);
      return;
    }
    const c = this.o.read();
    const fileUnchecked: string[] = [];
    let registered: boolean | null = true;
    if (!c.server) {
      if (force || !this.registered || now - this.registered.at >= CLAUDE_MEKO_TTL_MS) {
        const value = await this.o.mcpRegistered();
        // An answer is kept; not being able to ask is tried again next time.
        this.registered = value === null ? null : { at: now, value };
        registered = value;
      } else {
        registered = this.registered.value;
      }
      if (registered === null) fileUnchecked.push("Couldn't ask Claude Code whether it has a meko MCP server (`claude mcp get meko` didn't answer).");
    }
    if (c.server?.unresolved) {
      fileUnchecked.push(`Couldn't check ${PLACE.server}: it comes from \${${c.server.unresolved}}, which AgentK can't see (it isn't in Claude Code's settings env).`);
    }
    const gaps = mekoClaudeGaps(c, url, () => registered);
    const tokens = claudeMekoTokens(c, url).filter((t) => t.token !== apiKey);
    const required = this.o.projectDatapacks();
    const keyOf = (token: string) => [url, hash(apiKey), hash(token), required.map((d) => d.id).join(',')].join('\n');
    const saved = (token: string) => (force ? undefined : this.results.get(keyOf(token))?.outcome);
    if (tokens.some((t) => !saved(t.token))) this.set({ ...this.status, checking: true });

    const outcomes = new Map<string, TokenOutcome>();
    for (const t of tokens) {
      if (outcomes.has(t.token)) continue;
      let outcome = saved(t.token);
      if (!outcome) {
        outcome = await this.check(url, apiKey, t.token, required, now, force);
        if (outcome.kind !== 'unchecked') this.results.set(keyOf(t.token), { at: now, outcome });
      }
      outcomes.set(t.token, outcome);
    }
    const tokenGaps: string[] = [];
    const unchecked: string[] = [...fileUnchecked];
    for (const t of tokens) {
      const outcome = outcomes.get(t.token)!;
      const text = tokenOutcomeText(t.place, outcome, url);
      if (text) (outcome.kind === 'unchecked' ? unchecked : tokenGaps).push(text);
    }
    this.set({ gaps: [...gaps, ...tokenGaps], unchecked, checking: false });
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
