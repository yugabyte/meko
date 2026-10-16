import { useLayoutEffect, useRef, useState } from 'react';
import type { DailyUsage, ProjectUsage, TokenUsage, UsageCost } from '../../shared/types';
import { STAGE_LABELS } from '../../shared/types';
import { navigate } from '../router';
import { cx, formatCostOf, formatTokens, totalTokens, usageTitle } from '../util';
import { MekoUsageSection } from './MekoUsageSection';

/**
 * Token usage for a project: headline numbers, a 14-day chart, Meko use, and per-session and
 * per-model tables. `mekoOn`: Meko is on and the project has a datapack, so the Meko section shows
 * before any call is made.
 */
export function UsagePanel({ projectId, usage, mekoOn }: { projectId: string; usage: ProjectUsage; mekoOn: boolean }) {
  const [showTable, setShowTable] = useState(false);
  const t = usage.total;
  const empty = t.messages === 0;
  const showMeko = usage.meko.calls > 0 || usage.meko.savings.seeds > 0 || usage.mekoLearning.runs > 0 || mekoOn;
  const mekoColumn = usage.bySession.some((s) => s.meko.lookups + s.meko.saves > 0);
  return (
    <section className="box usage-box">
      <div className="usage-hd">
        <span className="label">Token usage</span>
        <span className="muted small">From Claude Code transcripts, subagents included</span>
      </div>
      {empty ? (
        <p className="muted">No usage yet. It's counted after each Claude turn.</p>
      ) : (
        <>
          <div className="usage-stats">
            <Stat label="Total" value={totalTokens(t)} strong />
            <CostStat cost={usage.cost} />
            <Stat label="Output" value={t.output} />
            <Stat label="Input" value={t.input} />
            <Stat label="Cache write" value={t.cacheWrite} />
            <Stat label="Cache read" value={t.cacheRead} />
            <div className="usage-stat">
              <span className="usage-num">{t.messages.toLocaleString()}</span>
              <span className="usage-lbl">Responses</span>
            </div>
          </div>

          <div className="usage-chart-hd">
            <span className="small">Total tokens per day, last 14 days</span>
            <button className="link-btn small" onClick={() => setShowTable((v) => !v)} aria-expanded={showTable}>
              {showTable ? 'Show chart' : 'Show table'}
            </button>
          </div>
          {showTable ? <DailyTable daily={usage.daily} /> : <DailyChart daily={usage.daily} />}

          {showMeko && (
            <div className="usage-meko">
              <MekoUsageSection
                meko={usage.meko}
                firstTurn={usage.mekoFirstTurn}
                learning={usage.mekoLearning}
                gaps={{ projectId, counts: usage.mekoGaps }}
              />
            </div>
          )}

          <div className="usage-tables">
            <table className="usage-table">
              <thead>
                <tr>
                  <th>Session</th>
                  {mekoColumn && <th className="num" title="Meko lookups that found something, of all lookups">Meko</th>}
                  <th className="num">Output</th>
                  <th className="num">Total</th>
                </tr>
              </thead>
              <tbody>
                {usage.bySession.map((s) => (
                  <tr key={s.sessionId} title={usageTitle(s.usage)}>
                    <td>
                      {s.archived ? (
                        <span>{s.name}</span>
                      ) : (
                        <button className="link-btn inline" onClick={() => navigate({ view: 'project', projectId, sessionId: s.sessionId })}>
                          {s.name}
                        </button>
                      )}{' '}
                      <span className="muted small">{s.archived ? 'archived' : STAGE_LABELS[s.stage].toLowerCase()}</span>
                    </td>
                    {mekoColumn && (
                      <td
                        className="num"
                        title={`${s.meko.found} of ${s.meko.lookups} Meko lookups found something · ${s.meko.saves} ${s.meko.saves === 1 ? 'save' : 'saves'}`}
                      >
                        {s.meko.lookups > 0 ? `${s.meko.found}/${s.meko.lookups}` : '—'}
                      </td>
                    )}
                    <td className="num">{formatTokens(s.usage.output)}</td>
                    <td className="num">{formatTokens(totalTokens(s.usage))}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            <table className="usage-table">
              <thead>
                <tr>
                  <th>Model</th>
                  <th className="num">Output</th>
                  <th className="num">Total</th>
                </tr>
              </thead>
              <tbody>
                {usage.byModel.map((m) => (
                  <tr key={m.model} title={usageTitle(m.usage)}>
                    <td className="mono small">{m.model}</td>
                    <td className="num">{formatTokens(m.usage.output)}</td>
                    <td className="num">{formatTokens(totalTokens(m.usage))}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </section>
  );
}

export function Stat({ label, value, strong }: { label: string; value: number; strong?: boolean }) {
  return (
    <div className="usage-stat" title={`${value.toLocaleString()} tokens`}>
      <span className={cx('usage-num', strong && 'strong')}>{formatTokens(value)}</span>
      <span className="usage-lbl">{label}</span>
    </div>
  );
}

/** What the tokens would cost at Anthropic's API list prices; partial when some models have no price. */
export function CostStat({ cost }: { cost: UsageCost }) {
  const partial = cost.unpricedMessages > 0;
  const left = partial
    ? ` Leaves out ${cost.unpricedMessages.toLocaleString()} ${cost.unpricedMessages === 1 ? 'response' : 'responses'} from models without a known price.`
    : '';
  return (
    <div className="usage-stat" title={`At Anthropic's API list prices.${left}`}>
      <span className="usage-num">{formatCostOf(cost)}</span>
      <span className="usage-lbl">{partial ? 'Est. cost, partial' : 'Est. cost'}</span>
    </div>
  );
}

/** Rounds up to 1, 2 or 5 × 10^k so the top gridline is a readable number. */
function niceMax(v: number): number {
  if (v <= 0) return 1;
  const p = 10 ** Math.floor(Math.log10(v));
  const m = v / p;
  return (m <= 1 ? 1 : m <= 2 ? 2 : m <= 5 ? 5 : 10) * p;
}

function dayLabel(day: string, style: 'short' | 'long'): string {
  const [y, m, d] = day.split('-').map(Number);
  const date = new Date(y, m - 1, d);
  return style === 'short'
    ? String(d)
    : date.toLocaleDateString(undefined, { weekday: 'short', month: 'short', day: 'numeric' });
}

export function DailyChart({ daily }: { daily: DailyUsage[] }) {
  const [hover, setHover] = useState<number | null>(null);
  const totals = daily.map((d) => totalTokens(d.usage));
  const max = niceMax(Math.max(...totals));
  const ticks = [max, max / 2, 0];
  const h = hover === null ? null : daily[hover];
  return (
    <div className="chart" role="img" aria-label={`Daily token totals: ${daily.map((d, i) => `${d.day} ${totals[i]}`).join(', ')}`}>
      <div className="chart-y" aria-hidden="true">
        {ticks.map((t) => (
          <span key={t}>{formatTokens(t)}</span>
        ))}
      </div>
      <div className="chart-plot" onMouseLeave={() => setHover(null)}>
        <div className="chart-grid" aria-hidden="true">
          {ticks.map((t) => (
            <span key={t} />
          ))}
        </div>
        <div className="chart-bars">
          {daily.map((d, i) => (
            <div
              key={d.day}
              className={cx('chart-col', hover === i && 'hover')}
              onMouseEnter={() => setHover(i)}
              onFocus={() => setHover(i)}
              onBlur={() => setHover(null)}
              tabIndex={0}
              aria-label={`${dayLabel(d.day, 'long')}: ${totals[i].toLocaleString()} tokens`}
            >
              <div className="chart-bar" style={{ height: `${(totals[i] / max) * 100}%` }} />
            </div>
          ))}
        </div>
        {h && hover !== null && <Tooltip day={h.day} usage={h.usage} index={hover} count={daily.length} />}
      </div>
      <div className="chart-x" aria-hidden="true">
        {daily.map((d, i) => (
          <span key={d.day}>{i % 2 === daily.length % 2 ? '' : dayLabel(d.day, 'short')}</span>
        ))}
      </div>
    </div>
  );
}

/** Beside the bar, on whichever side has room; over the bars, kept inside the plot, when neither has (phones). */
function Tooltip({ day, usage, index, count }: { day: string; usage: TokenUsage; index: number; count: number }) {
  const ref = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => {
    const tip = ref.current;
    const plot = tip?.parentElement;
    if (!tip || !plot) return;
    const room = plot.clientWidth;
    const w = tip.offsetWidth;
    const x = ((index + 0.5) / count) * room;
    const gap = 12;
    const left = x + gap + w <= room ? x + gap : x - gap - w >= 0 ? x - gap - w : Math.max(0, Math.min(room - w, x - w / 2));
    tip.style.left = `${left}px`;
  }, [index, count, day, usage]);
  const f = (n: number) => n.toLocaleString();
  return (
    <div className="chart-tip" ref={ref}>
      <div className="chart-tip-hd">{dayLabel(day, 'long')}</div>
      <div className="chart-tip-total">{f(totalTokens(usage))} tokens</div>
      <dl>
        <dt>Output</dt>
        <dd>{f(usage.output)}</dd>
        <dt>Input</dt>
        <dd>{f(usage.input)}</dd>
        <dt>Cache write</dt>
        <dd>{f(usage.cacheWrite)}</dd>
        <dt>Cache read</dt>
        <dd>{f(usage.cacheRead)}</dd>
        <dt>Responses</dt>
        <dd>{f(usage.messages)}</dd>
      </dl>
    </div>
  );
}

export function DailyTable({ daily }: { daily: DailyUsage[] }) {
  return (
    <div className="table-scroll">
      <table className="usage-table">
        <thead>
          <tr>
            <th>Day</th>
            <th className="num">Output</th>
            <th className="num">Input</th>
            <th className="num">Cache write</th>
            <th className="num">Cache read</th>
            <th className="num">Total</th>
          </tr>
        </thead>
        <tbody>
          {[...daily].reverse().map((d) => (
            <tr key={d.day}>
              <td>{dayLabel(d.day, 'long')}</td>
              <td className="num">{formatTokens(d.usage.output)}</td>
              <td className="num">{formatTokens(d.usage.input)}</td>
              <td className="num">{formatTokens(d.usage.cacheWrite)}</td>
              <td className="num">{formatTokens(d.usage.cacheRead)}</td>
              <td className="num">{formatTokens(totalTokens(d.usage))}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
