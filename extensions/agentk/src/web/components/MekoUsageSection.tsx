import type { MekoFirstTurn, MekoGapCounts, MekoLearning, MekoSavings, MekoUsage } from '../../shared/types';
import { appUrl } from '../base';
import { navigate, routePath } from '../router';
import { cx, firstTurnText, formatCost, formatTokens, gapsLine, learningText, type LearningTile } from '../util';

const ESTIMATE =
  'Estimate: each Meko search that found something, and each seed AgentK passed at session start, is credited with the exploring it most likely replaced ' +
  '(3 tool calls, each re-reading the context and adding about 2,000 tokens). Anthropic API list prices.';

/**
 * Meko lookups and saves Claude made, what they're estimated to have saved, how many tokens Meko brought into
 * sessions' first turns, and what the learn step saved and cost: the project home and the Usage page. `gaps`
 * (the project home only): the questions Meko couldn't answer, as a line under the stats linking to What
 * Meko doesn't know.
 */
export function MekoUsageSection({
  meko,
  firstTurn,
  learning,
  gaps,
}: {
  meko: MekoUsage;
  firstTurn: MekoFirstTurn;
  learning: MekoLearning;
  gaps?: { projectId: string; counts: MekoGapCounts };
}) {
  const learned = learningText(learning);
  const f = (n: number) => n.toLocaleString();
  return (
    <div className="meko-usage">
      <div className="meko-usage-hd">
        {/* One logo per theme; CSS shows the one that reads on the background. */}
        <img className="meko-logo on-light" src={appUrl('/meko-black.png')} alt="Meko" width={56} height={18} />
        <img className="meko-logo on-dark" src={appUrl('/meko-white.png')} alt="Meko" width={56} height={18} />
      </div>
      <div className="usage-stats">
        <div className="usage-stat" title={`${f(meko.found)} found something, ${f(meko.empty)} found nothing, ${f(meko.errors)} failed (saves included)`}>
          <span className="usage-num">{f(meko.lookups)}</span>
          <span className="usage-lbl">Lookups</span>
        </div>
        <div className="usage-stat">
          <span className="usage-num">{f(meko.found)}</span>
          <span className="usage-lbl">Found something</span>
        </div>
        <div className="usage-stat">
          <span className="usage-num">{f(meko.saves)}</span>
          <span className="usage-lbl">Saves</span>
        </div>
        <FirstTurnStat firstTurn={firstTurn} />
        <div className="usage-stat" title={`About ${f(meko.foundTokens)} tokens (characters ÷ 4)`}>
          <span className="usage-num">≈ {formatTokens(meko.foundTokens)}</span>
          <span className="usage-lbl">Tokens found</span>
        </div>
        <SavedStat savings={meko.savings} />
        <LearningStat tile={learned.learned} label="Learned" />
        <LearningStat tile={learned.cost} label="Learning cost" />
      </div>
      {gaps && <GapsLine projectId={gaps.projectId} counts={gaps.counts} />}
    </div>
  );
}

/** "9 questions Meko couldn't answer · 5 found nothing · 4 nothing relevant", linking to What Meko doesn't know; nothing when there are none. */
function GapsLine({ projectId, counts }: { projectId: string; counts: MekoGapCounts }) {
  const line = gapsLine(counts);
  if (!line) return null;
  const route = { view: 'meko-gaps', projectId } as const;
  return (
    <a
      className="meko-gaps"
      href={routePath(route)}
      onClick={(e) => {
        if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
        e.preventDefault();
        navigate(route);
      }}
    >
      <span className="meko-gaps-text">
        <b>{line.count}</b> Meko couldn't answer <span className="muted">· {line.split}</span>
      </span>
      <span className="meko-gaps-link">See what Meko doesn't know ›</span>
    </a>
  );
}

function SavedStat({ savings: s }: { savings: MekoSavings }) {
  if (!credited(s)) {
    return (
      <div className="usage-stat" title="No Meko search has found anything, and AgentK hasn't seeded a session, yet.">
        <span className="usage-num muted">—</span>
        <span className="usage-lbl">Est. saved</span>
      </div>
    );
  }
  const title = `${ESTIMATE} ${s.tokens.toLocaleString()} tokens from ${countText(s.searches, s.seeds)}.`;
  // Dollars that leave out searches or seeds on unpriced models say so in the label (tooltips aren't seen on touch).
  const partial = s.usd !== null && s.unpricedSearches + s.unpricedSeeds > 0;
  return (
    <div className={cx('usage-stat', savedClass(s))} title={title}>
      <span className="usage-num">{s.usd !== null ? formatCost(s.usd) : `≈ ${signedTokens(s.tokens)}`}</span>
      <span className="usage-lbl">{s.usd !== null ? `Est. saved · ≈ ${signedTokens(s.tokens)} tokens` : 'Est. tokens saved'}</span>
      {partial && (
        <span className="usage-lbl">
          $ leaves out {countText(s.unpricedSearches, s.unpricedSeeds, 'unpriced ')}
        </span>
      )}
    </div>
  );
}

/** A learn-step tile (`learningText`): a dash before any step ran. */
function LearningStat({ tile, label }: { tile: LearningTile; label: string }) {
  return (
    <div className="usage-stat" title={tile.title}>
      <span className={cx('usage-num', tile.num === '—' && 'muted')}>{tile.num}</span>
      <span className="usage-lbl">{label}</span>
      {tile.line && <span className="usage-lbl">{tile.line}</span>}
    </div>
  );
}

/** Tokens Meko brought into first turns, split into the seed AgentK passed at start and Claude's searches (`firstTurnText`). */
function FirstTurnStat({ firstTurn: t }: { firstTurn: MekoFirstTurn }) {
  const { split, title } = firstTurnText(t);
  return (
    <div className="usage-stat" title={title}>
      {t.sessions === 0
        ? <span className="usage-num muted">—</span>
        : <span className="usage-num">≈ {formatTokens(t.searchedTokens + t.seedTokens)}</span>}
      <span className="usage-lbl">1st-turn tokens</span>
      {split && <span className="usage-lbl">{split}</span>}
    </div>
  );
}

/** Green only for a saving; a negative estimate keeps the normal text color. */
export function savedClass(s: MekoSavings): string | false {
  return credited(s) && (s.usd ?? s.tokens) > 0 && 'saved';
}

/** Est. saved in a table cell: cost, tokens when no model had a price, or a dash with no search to count. */
export function savedText(s: MekoSavings): string {
  if (!credited(s)) return '—';
  return s.usd !== null ? formatCost(s.usd) : `≈ ${signedTokens(s.tokens)}`;
}

/** Tooltip for an Est. saved cell: the tokens behind it, and searches or seeds its dollars leave out. */
export function savedTitle(s: MekoSavings): string | undefined {
  if (!credited(s)) return undefined;
  const left = s.usd !== null && s.unpricedSearches + s.unpricedSeeds > 0
    ? `; the dollars leave out ${countText(s.unpricedSearches, s.unpricedSeeds)} on models without a known price` : '';
  return `≈ ${s.tokens.toLocaleString()} tokens from ${countText(s.searches, s.seeds)}${left}`;
}

/** Whether anything was credited: a search that found something or a seed. */
export function credited(s: MekoSavings): boolean {
  return s.searches + s.seeds > 0;
}

/** Searches and seeds in words, leaving out a zero: "3 searches and 1 seed", "2 unpriced seeds". */
function countText(searches: number, seeds: number, adjective = ''): string {
  const n = (k: number, one: string, many: string) => `${k.toLocaleString()} ${adjective}${k === 1 ? one : many}`;
  return [searches > 0 && n(searches, 'search', 'searches'), seeds > 0 && n(seeds, 'seed', 'seeds')].filter(Boolean).join(' and ');
}

function signedTokens(n: number): string {
  return n < 0 ? `−${formatTokens(-n)}` : formatTokens(n);
}
