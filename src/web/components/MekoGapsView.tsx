import { useEffect, useRef, useState } from 'react';
import type { MekoGap, MekoGapPage, MekoGapState, ProjectWithSessions } from '../../shared/types';
import { api, errorMessage } from '../api';
import { copyText } from '../clipboard';
import { useIsMobile } from '../hooks/useMedia';
import { navigate } from '../router';
import { askedText, cx, GAP_QUALITY_LABEL } from '../util';
import { Busy } from './common';
import { TeachMekoDialog } from './TeachMekoDialog';
import { TopBar } from './TopBar';

type Filter = MekoGapState | 'all';
const FILTERS: { id: Filter; label: string }[] = [
  { id: 'open', label: 'Open' },
  { id: 'taught', label: 'Taught' },
  { id: 'dismissed', label: 'Dismissed' },
  { id: 'all', label: 'All' },
];
/** A query longer than this is cut on its row until it's opened. */
const SHORT_QUERY = 240;

const day = (ms: number) => new Date(ms).toLocaleDateString([], { month: 'short', day: 'numeric' });

/**
 * What Meko doesn't know (`/p/:id/meko-gaps`): the project's Meko searches that found nothing or
 * nothing relevant, grouped by question, latest asked first, each with Teach Meko, Copy and
 * Dismiss (or Restore). Reached from the line under the Meko stats on the project home.
 */
export function MekoGapsView({
  project,
  mekoConfigured,
  onMenu,
  onToast,
}: {
  project: ProjectWithSessions;
  mekoConfigured: boolean;
  onMenu: () => void;
  onToast: (m: string) => void;
}) {
  const mobile = useIsMobile();
  const [filter, setFilter] = useState<Filter>('open');
  const [page, setPage] = useState<MekoGapPage | null>(null);
  const [gaps, setGaps] = useState<MekoGap[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [reload, setReload] = useState(0);
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [teaching, setTeaching] = useState<MekoGap | null>(null);
  const [busyKey, setBusyKey] = useState<string | null>(null);
  // Bumped whenever the list is loaded afresh: a Load more answer for a view that's gone is dropped.
  const view = useRef(0);
  const dedicated = project.datapacks.find((d) => d.role === 'dedicated') ?? null;
  const teachBlocker = !mekoConfigured
    ? "Meko is off, so it can't be taught. Add a Meko API key on the Meko tab in Settings."
    : !dedicated
      ? 'This project has no dedicated datapack to save to. Add one on the project home.'
      : null;

  useEffect(() => {
    let gone = false;
    view.current++;
    setLoading(true);
    api
      .mekoGaps(project.id, { state: filter })
      .then((p) => {
        if (gone) return;
        setPage(p);
        setGaps(p.gaps);
        setError(null);
      })
      .catch((e) => !gone && setError(errorMessage(e)))
      .finally(() => !gone && setLoading(false));
    return () => {
      gone = true;
    };
  }, [project.id, filter, reload]);

  const more = async () => {
    const last = gaps?.at(-1);
    if (!last) return;
    const asked = view.current;
    setLoading(true);
    try {
      const p = await api.mekoGaps(project.id, { state: filter, after: { lastAt: last.lastAt, key: last.key } });
      if (view.current !== asked) return;
      setGaps((g) => [...(g ?? []), ...p.gaps]);
      setPage((old) => (old ? { ...old, more: p.more } : p));
    } catch (e) {
      if (view.current === asked) onToast(errorMessage(e));
    } finally {
      if (view.current === asked) setLoading(false);
    }
  };

  const mark = async (g: MekoGap, state: 'dismissed' | null) => {
    setBusyKey(g.key);
    try {
      await api.markMekoGap(project.id, { key: g.key, state });
      setReload((n) => n + 1);
    } catch (e) {
      onToast(errorMessage(e));
    } finally {
      setBusyKey(null);
    }
  };

  const copy = async (text: string, what: string) => {
    if (await copyText(text)) onToast(`Copied ${what}.`);
    else onToast("Couldn't copy. Select the text and copy it instead.");
  };

  const back = () => navigate({ view: 'home', projectId: project.id });
  const open = page ? page.open.empty + page.open.weak : 0;
  return (
    <div className="home-view">
      {mobile && (
        <TopBar onMenu={onMenu} sub={project.name} title="What Meko doesn't know">
          <button className="btn small" onClick={back}>
            Project home
          </button>
        </TopBar>
      )}
      <header className="home-hd">
        {!mobile && (
          <button className="link-btn" onClick={back}>
            ← Project home
          </button>
        )}
        {!mobile && <h1>What Meko doesn't know</h1>}
        <span className="muted small">
          Searches in this project's sessions that found nothing, or found only memories scoring below 0.75. Off-topic memories that score higher still
          count as found.
        </span>
      </header>
      <div className="home-scroll">
        <section className="box mg-box">
          {page && (
            <p className="mg-sum muted small">
              {page.since === null
                ? 'No searches recorded yet. Searches are recorded from now on, as sessions make them.'
                : `Since ${day(page.since)}: ${page.searches.toLocaleString()} ${page.searches === 1 ? 'search' : 'searches'}, ` +
                  `${open.toLocaleString()} open ${open === 1 ? 'question' : 'questions'}`}
            </p>
          )}
          <div className="mg-bar">
            <div className="mg-filters" role="group" aria-label="Show">
              {FILTERS.map((f) => (
                <button
                  key={f.id}
                  className={cx('mg-filter', filter === f.id && 'on')}
                  aria-pressed={filter === f.id}
                  onClick={() => setFilter(f.id)}
                >
                  {f.label}
                  {page && f.id !== 'all' ? ` ${page.counts[f.id].toLocaleString()}` : ''}
                </button>
              ))}
            </div>
            <span className="sp" />
            {!!gaps?.length && (
              <button className="btn small" onClick={() => void copy(gaps.map((g) => `- ${g.query.replace(/\s+/g, ' ')}`).join('\n'), 'the list')}>
                Copy all
              </button>
            )}
          </div>
          {teachBlocker && <p className="muted small">{teachBlocker}</p>}
          {error && <p className="error-text">{error}</p>}
          {gaps === null ? (
            !error && <p className="muted"><Busy label="Loading" /></p>
          ) : !gaps.length ? (
            <p className="muted">{filter === 'open' ? 'Nothing open. Every search recorded found something relevant, or was dealt with.' : 'None.'}</p>
          ) : (
            <div className="mg-list">
              {gaps.map((g) => {
                const long = g.query.length > SHORT_QUERY && !expanded.has(g.key);
                return (
                  <div key={g.key} className="mg-gap">
                    <div className="mg-top">
                      <span className="mg-query">{long ? `${g.query.slice(0, SHORT_QUERY)}…` : g.query}</span>
                      <span className="mg-chips">
                        <span className={cx('mg-chip', g.quality)}>{GAP_QUALITY_LABEL[g.quality]}</span>
                        {g.state !== 'open' && <span className={cx('mg-chip', g.state)}>{g.state === 'taught' ? 'Taught' : 'Dismissed'}</span>}
                      </span>
                    </div>
                    <div className="mg-meta">
                      {askedText(g.asks)}
                      {' · '}
                      {g.sessions.map((s, i) => (
                        <span key={s.id}>
                          {i > 0 && ', '}
                          {s.archived ? (
                            s.name
                          ) : (
                            <button className="link-btn" onClick={() => navigate({ view: 'project', projectId: project.id, sessionId: s.id })}>
                              {s.name}
                            </button>
                          )}
                        </span>
                      ))}
                      {` · ${g.asks > 1 ? 'last ' : ''}${day(g.lastAt)} · `}
                      {[
                        g.source === 'start' ? "AgentK's search at start" : g.tool,
                        g.quality === 'weak' && g.results !== null && `${g.results.toLocaleString()} found`,
                        // The best score and the best raw similarity may be different memories'.
                        g.topScore !== null && `best score ${g.topScore.toFixed(2)}`,
                        g.topRaw !== null && `best raw ${g.topRaw.toFixed(2)}`,
                      ].filter(Boolean).join(' · ')}
                    </div>
                    {g.taught && (
                      <div className="mg-taught">
                        <span className="muted small">
                          {g.state === 'taught'
                            ? `Taught ${day(g.taught.at)}${g.taught.datapack ? ` in ${g.taught.datapack}` : ''}:`
                            : `Taught on ${day(g.taught.at)}, and asked again since. Meko had this and the search still missed it:`}
                        </span>
                        <span className="mg-taught-text">{g.taught.text}</span>
                      </div>
                    )}
                    <div className="mg-actions">
                      {g.query.length > SHORT_QUERY && (
                        <button
                          className="link-btn"
                          aria-expanded={!long}
                          onClick={() =>
                            setExpanded((x) => {
                              const n = new Set(x);
                              if (n.has(g.key)) n.delete(g.key);
                              else n.add(g.key);
                              return n;
                            })
                          }
                        >
                          {long ? 'Show all' : 'Show less'}
                        </button>
                      )}
                      {g.state === 'open' && (
                        <button className="btn small primary" disabled={!!teachBlocker} title={teachBlocker ?? undefined} onClick={() => setTeaching(g)}>
                          Teach Meko
                        </button>
                      )}
                      <button className="btn small" onClick={() => void copy(g.query, 'the question')}>
                        Copy
                      </button>
                      {g.state === 'open' ? (
                        <button className="btn small" disabled={busyKey === g.key} onClick={() => void mark(g, 'dismissed')}>
                          {busyKey === g.key ? <Busy label="Dismissing" /> : 'Dismiss'}
                        </button>
                      ) : (
                        <button className="btn small" disabled={busyKey === g.key} onClick={() => void mark(g, null)}>
                          {busyKey === g.key ? <Busy label="Restoring" /> : 'Restore'}
                        </button>
                      )}
                    </div>
                  </div>
                );
              })}
              {page?.more && (
                <div className="mc-more">
                  <button className="btn small" disabled={loading} onClick={() => void more()}>
                    {loading ? <Busy label="Loading" /> : 'Load more'}
                  </button>
                </div>
              )}
            </div>
          )}
        </section>
      </div>
      {teaching && dedicated && (
        <TeachMekoDialog
          projectId={project.id}
          gap={teaching}
          datapack={dedicated.name}
          onClose={() => setTeaching(null)}
          onTaught={() => {
            setTeaching(null);
            onToast(`Saved to ${dedicated.name}.`);
            setReload((n) => n + 1);
          }}
        />
      )}
    </div>
  );
}
