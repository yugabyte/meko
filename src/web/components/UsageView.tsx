import { useEffect, useMemo, useState } from 'react';
import type { ModelUsage, ProjectUsageSummary, UsageOverview } from '../../shared/types';
import { api, errorMessage } from '../api';
import { useIsMobile } from '../hooks/useMedia';
import { navigate } from '../router';
import { nextSort, sortRows, type SortState } from '../sort';
import { cx, formatCostOf, formatTokens, relativeTime, repoFolder, totalTokens, usageTitle } from '../util';
import { MekoUsageSection, savedClass, savedText, savedTitle } from './MekoUsageSection';
import { TopBar } from './TopBar';
import { CostStat, DailyChart, DailyTable, Stat } from './UsagePanel';

const plural = (n: number, word: string) => `${n.toLocaleString()} ${word}${n === 1 ? '' : 's'}`;

type ProjectKey = 'name' | 'sessions' | 'mergedPrs' | 'directCommits' | 'output' | 'total' | 'cost' | 'meko' | 'saved';
type ModelKey = 'model' | 'output' | 'total';

const projectValue = (p: ProjectUsageSummary, k: ProjectKey): number | string => {
  switch (k) {
    case 'name':
      return p.name;
    case 'output':
      return p.usage.output;
    case 'total':
      return totalTokens(p.usage);
    case 'cost':
      return p.cost.usd;
    case 'meko':
      return p.meko.found;
    case 'saved': {
      // No estimate sorts last; a tokens-only one (no known price) sorts by its tokens among $0.
      const s = p.meko.savings;
      return s.searches === 0 ? -Number.MAX_VALUE : (s.usd ?? 0) + s.tokens * 1e-12;
    }
    default:
      return p[k];
  }
};
const modelValue = (m: ModelUsage, k: ModelKey): number | string =>
  k === 'model' ? m.model : k === 'output' ? m.usage.output : totalTokens(m.usage);

/**
 * Usage across every project, deleted ones included: tokens and their cost by project, model and
 * day, Meko use and its estimated savings, and what shipped (merged PRs, and commits pushed
 * straight to a base branch). `mekoConfigured`: Meko is on, so the Meko section shows before any call.
 */
export function UsageView({ onMenu, mekoConfigured }: { onMenu: () => void; mekoConfigured: boolean }) {
  const mobile = useIsMobile();
  const [usage, setUsage] = useState<UsageOverview | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [showTable, setShowTable] = useState(false);
  // Until a column is picked, projects keep the server's order: current ones first, largest first.
  const [projectSort, setProjectSort] = useState<SortState<ProjectKey>>({ key: null, dir: 'desc' });
  const [modelSort, setModelSort] = useState<SortState<ModelKey>>({ key: null, dir: 'desc' });
  const projects = useMemo(() => sortRows(usage?.byProject ?? [], projectSort, projectValue), [usage, projectSort]);
  const models = useMemo(() => sortRows(usage?.byModel ?? [], modelSort, modelValue), [usage, modelSort]);
  const projectHeader = (key: ProjectKey, label: string, opts: { text?: boolean; className?: string; title?: string } = {}) => (
    <SortHeader sort={projectSort} col={key} text={opts.text} className={opts.className} title={opts.title} label={label} onSort={() => setProjectSort((cur) => nextSort(cur, key, !!opts.text))} />
  );
  const modelHeader = (key: ModelKey, label: string, opts: { text?: boolean; className?: string } = {}) => (
    <SortHeader sort={modelSort} col={key} text={opts.text} className={opts.className} label={label} onSort={() => setModelSort((cur) => nextSort(cur, key, !!opts.text))} />
  );

  useEffect(() => {
    api
      .usage()
      .then((u) => {
        setUsage(u);
        setError(null);
      })
      .catch((e) => setError(errorMessage(e)));
  }, []);

  const t = usage?.total;
  const showMeko = !!usage && (usage.meko.calls > 0 || mekoConfigured);
  return (
    <div className="home-view">
      {mobile && <TopBar onMenu={onMenu} title="Usage" />}
      <header className="home-hd">
        {!mobile && <h1>Usage</h1>}
        <span className="muted small">Every project, deleted ones included. Tokens come from Claude Code transcripts, subagents included.</span>
      </header>
      <div className="home-scroll">
        {error && <p className="error-text">{error}</p>}
        {!usage || !t ? (
          !error && <p className="muted">Loading…</p>
        ) : (
          <>
            <section className="box">
              <span className="label">Totals</span>
              <div className="usage-stats">
                <Stat label="Total tokens" value={totalTokens(t)} strong />
                <CostStat cost={usage.cost} />
                <Stat label="Output" value={t.output} />
                <Stat label="Input" value={t.input} />
                <Stat label="Cache write" value={t.cacheWrite} />
                <Stat label="Cache read" value={t.cacheRead} />
                <Count label="Merged PRs" value={usage.mergedPrs} />
                <Count
                  label="Commits pushed to base"
                  value={usage.directCommits}
                  note={usage.directPushes > 0 ? `in ${plural(usage.directPushes, 'push')}` : undefined}
                />
              </div>
              <div className="usage-chart-hd">
                <span className="small">Total tokens per day, last 14 days</span>
                <button className="link-btn small" onClick={() => setShowTable((v) => !v)} aria-expanded={showTable}>
                  {showTable ? 'Show chart' : 'Show table'}
                </button>
              </div>
              {showTable ? <DailyTable daily={usage.daily} /> : <DailyChart daily={usage.daily} />}
            </section>

            {showMeko && (
              <section className="box">
                <MekoUsageSection meko={usage.meko} />
              </section>
            )}

            <section className="box">
              <span className="label">By project</span>
              {usage.byProject.length === 0 ? (
                <p className="muted">No projects yet.</p>
              ) : (
                <>
                  <div className="table-scroll">
                    <table className="usage-table usage-projects">
                      <thead>
                        <tr>
                          {projectHeader('name', 'Project', { text: true })}
                          {projectHeader('sessions', 'Sessions', { className: 'num opt' })}
                          {projectHeader('mergedPrs', 'Merged PRs', { className: 'num', title: 'Sessions whose PR was merged' })}
                          {projectHeader('directCommits', 'Pushed', { className: 'num', title: "Commits a session pushed straight to the project's base branch" })}
                          {projectHeader('output', 'Output', { className: 'num opt' })}
                          {projectHeader('total', 'Total', { className: 'num' })}
                          {projectHeader('cost', 'Cost', { className: 'num opt', title: "What the tokens would cost at Anthropic's API list prices" })}
                          {showMeko && projectHeader('meko', 'Meko', { className: 'num opt', title: 'Meko lookups that found something, of all lookups' })}
                          {showMeko && projectHeader('saved', 'Est. saved', { className: 'num opt', title: 'Estimated from the Meko searches that found something' })}
                        </tr>
                      </thead>
                      <tbody>
                        {projects.map((p) => (
                          <tr key={p.projectId} className={cx(p.deleted && 'deleted')} title={usageTitle(p.usage)}>
                            <td>
                              {p.deleted ? (
                                <span>{p.name}</span>
                              ) : (
                                <button className="link-btn inline" onClick={() => navigate({ view: 'home', projectId: p.projectId })}>
                                  {p.name}
                                </button>
                              )}{' '}
                              {p.deleted && <span className="tag">deleted</span>}
                              <span className="muted small usage-when">
                                <span className="mono" title={p.repoPath}>
                                  {repoFolder(p.repoPath)}
                                </span>
                                {' · '}
                                <span>{p.lastActiveAt ? `active ${relativeTime(p.lastActiveAt).toLowerCase()}` : 'no usage yet'}</span>
                              </span>
                            </td>
                            <td className="num opt">{p.sessions.toLocaleString()}</td>
                            <td className="num">{p.mergedPrs.toLocaleString()}</td>
                            <td className="num">
                              {p.directCommits.toLocaleString()}
                              {p.directPushes > 0 && <span className="muted small usage-when">in {plural(p.directPushes, 'push')}</span>}
                            </td>
                            <td className="num opt">{formatTokens(p.usage.output)}</td>
                            <td className="num">{formatTokens(totalTokens(p.usage))}</td>
                            <td
                              className="num opt"
                              title={p.cost.unpricedMessages > 0 ? `Leaves out ${plural(p.cost.unpricedMessages, 'response')} from models without a known price` : undefined}
                            >
                              {formatCostOf(p.cost)}
                            </td>
                            {showMeko && <td className="num opt">{p.meko.lookups > 0 ? `${p.meko.found}/${p.meko.lookups}` : '—'}</td>}
                            {showMeko && (
                              <td
                                className={cx('num opt', savedClass(p.meko.savings))}
                                title={savedTitle(p.meko.savings)}
                              >
                                {savedText(p.meko.savings)}
                              </td>
                            )}
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                  <p className="muted small usage-key">
                    Merged PRs: sessions whose PR was merged. Pushed: commits sessions pushed straight to the project’s base branch.
                    Cost: at Anthropic’s API list prices.
                  </p>
                </>
              )}
            </section>

            {usage.byModel.length > 0 && (
              <section className="box">
                <span className="label">By model</span>
                <table className="usage-table">
                  <thead>
                    <tr>
                      {modelHeader('model', 'Model', { text: true })}
                      {modelHeader('output', 'Output', { className: 'num' })}
                      {modelHeader('total', 'Total', { className: 'num' })}
                    </tr>
                  </thead>
                  <tbody>
                    {models.map((m) => (
                      <tr key={m.model} title={usageTitle(m.usage)}>
                        <td className="mono small">{m.model}</td>
                        <td className="num">{formatTokens(m.usage.output)}</td>
                        <td className="num">{formatTokens(totalTokens(m.usage))}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </section>
            )}
          </>
        )}
      </div>
    </div>
  );
}

/** A column header that sorts its table: tap to sort by it, tap again to reverse. */
function SortHeader<K extends string>({
  sort,
  col,
  text,
  className,
  title,
  label,
  onSort,
}: {
  sort: SortState<K>;
  col: K;
  text?: boolean;
  className?: string;
  title?: string;
  label: string;
  onSort: () => void;
}) {
  const on = sort.key === col;
  const arrow = on ? (sort.dir === 'asc' ? '▲' : '▼') : '';
  const next = on ? (sort.dir === 'asc' ? 'descending' : 'ascending') : text ? 'A to Z' : 'largest first';
  return (
    <th className={className} aria-sort={on ? (sort.dir === 'asc' ? 'ascending' : 'descending') : undefined} title={title}>
      <button type="button" className={cx('th-sort', on && 'on')} onClick={onSort} aria-label={`${label}: sort ${next}`}>
        {label}
        <span className="th-arrow" aria-hidden="true">
          {arrow}
        </span>
      </button>
    </th>
  );
}

/** A plain count in the stats row (Stat formats token amounts). */
function Count({ label, value, note }: { label: string; value: number; note?: string }) {
  return (
    <div className="usage-stat">
      <span className="usage-num">{value.toLocaleString()}</span>
      <span className="usage-lbl">{label}</span>
      {note && <span className="muted small">{note}</span>}
    </div>
  );
}
