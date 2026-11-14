import { mekoGapsPath } from '../../shared/mekourls';
import type { MekoFirstTurn, MekoGaps, MekoLearning, MekoSaved, MekoUsage } from '../../shared/types';
import { appUrl } from '../base';
import { cx, firstTurnText, formatTokens, gapsLine, learningText, mekoSavedText, type LearningTile } from '../util';

/**
 * Meko lookups and saves Claude made, how many tokens Meko brought into sessions' first turns, the
 * tokens Meko says its searches saved, and what the learn step saved and the tokens it used: the
 * project home and the Usage page. `gaps`
 * (the project home only): the open questions on Meko's Context Gaps for the project's dedicated datapack,
 * as a line under the stats linking to that tab in Meko's web UI, there with none open too.
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
  gaps?: MekoGaps | null;
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
        <MekoSavedStat saved={meko.mekoSaved} />
        <LearningStat tile={learned.learned} label="Learned" />
        <LearningStat tile={learned.tokens} label="Learning tokens" />
      </div>
      {gaps && <GapsLine gaps={gaps} />}
    </div>
  );
}

/**
 * "9 questions Meko couldn't answer · 5 found nothing · 4 nothing relevant", or "No open questions
 * Meko couldn't answer", opening the datapack's Context Gaps in Meko's web UI (on this origin: the
 * server sends the counts only while Kode uses the Meko on this computer) in a new tab.
 */
function GapsLine({ gaps }: { gaps: MekoGaps }) {
  const line = gapsLine(gaps);
  return (
    <a className="meko-gaps" href={mekoGapsPath(gaps.datapackId)} target="_blank" rel="noopener noreferrer">
      <span className="meko-gaps-text">
        {line
          ? <><b>{line.count}</b> Meko couldn't answer <span className="muted">· {line.split}</span></>
          : <span className="muted">No open questions Meko couldn't answer</span>}
      </span>
      <span className="meko-gaps-link">See Context Gaps in Meko ›</span>
    </a>
  );
}

/** The tokens Meko says its searches saved (`mekoSavedText`): a dash before any reply gave a figure. */
function MekoSavedStat({ saved }: { saved: MekoSaved }) {
  const t = mekoSavedText(saved);
  return (
    <div className="usage-stat" title={t.title}>
      <span className={cx('usage-num', t.num === '—' && 'muted')}>{t.num}</span>
      <span className="usage-lbl">Tokens saved</span>
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

/** Tokens Meko brought into first turns, split into the seed Kode passed at start and Claude's searches (`firstTurnText`). */
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

/** A Tokens saved cell is green for a saving. */
export function savedClass(m: MekoSaved): string | false {
  return m.tokens > 0 && 'saved';
}
