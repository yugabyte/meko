import type { MekoSavings, MekoUsage } from '../../shared/types';
import { cx, formatCost, formatTokens } from '../util';

const ESTIMATE =
  'Estimate: each Meko search that found something is credited with the exploring it most likely replaced ' +
  '(3 tool calls, each re-reading the context and adding about 2,000 tokens). Anthropic API list prices.';

/** Meko lookups and saves Claude made, and what they're estimated to have saved: the project home and the Usage page. */
export function MekoUsageSection({ meko }: { meko: MekoUsage }) {
  const f = (n: number) => n.toLocaleString();
  return (
    <div className="meko-usage">
      <div className="meko-usage-hd">
        {/* One logo per theme; CSS shows the one that reads on the background. */}
        <img className="meko-logo on-light" src="/meko-black.png" alt="Meko" width={56} height={18} />
        <img className="meko-logo on-dark" src="/meko-white.png" alt="Meko" width={56} height={18} />
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
        <div className="usage-stat" title={`About ${f(meko.foundTokens)} tokens (characters ÷ 4)`}>
          <span className="usage-num">≈ {formatTokens(meko.foundTokens)}</span>
          <span className="usage-lbl">Tokens found</span>
        </div>
        <SavedStat savings={meko.savings} />
      </div>
    </div>
  );
}

function SavedStat({ savings: s }: { savings: MekoSavings }) {
  if (s.searches === 0) {
    return (
      <div className="usage-stat" title="No Meko search has found anything yet.">
        <span className="usage-num muted">—</span>
        <span className="usage-lbl">Est. saved</span>
      </div>
    );
  }
  const title = `${ESTIMATE} ${s.tokens.toLocaleString()} tokens from ${s.searches.toLocaleString()} ${s.searches === 1 ? 'search' : 'searches'}.`;
  return (
    <div className={cx('usage-stat', savedClass(s))} title={title}>
      <span className="usage-num">{s.usd !== null ? formatCost(s.usd) : `≈ ${signedTokens(s.tokens)}`}</span>
      <span className="usage-lbl">{s.usd !== null ? `Est. saved · ≈ ${signedTokens(s.tokens)} tokens` : 'Est. tokens saved'}</span>
    </div>
  );
}

/** Green only for a saving; a negative estimate keeps the normal text color. */
export function savedClass(s: MekoSavings): string | false {
  return s.searches > 0 && (s.usd ?? s.tokens) > 0 && 'saved';
}

/** Est. saved in a table cell: cost, tokens when no model had a price, or a dash with no search to count. */
export function savedText(s: MekoSavings): string {
  if (s.searches === 0) return '—';
  return s.usd !== null ? formatCost(s.usd) : `≈ ${signedTokens(s.tokens)}`;
}

function signedTokens(n: number): string {
  return n < 0 ? `−${formatTokens(-n)}` : formatTokens(n);
}
