import { useEffect, useRef, useState } from 'react';
import { sideText } from '../../shared/tour';
import type { ProjectWithSessions, TourAction, TourLink, TourState } from '../../shared/types';
import { api, errorMessage } from '../api';
import { appUrl } from '../base';
import { navigate } from '../router';
import { cx } from '../util';
import { Busy } from './common';
import { MarkdownView } from './MarkdownView';

/** The links clicked on the card (Play the game), kept in this browser per project: once played, Move on is the blue button. */
const PLAYED_KEY = 'kode-tour-played';

function readPlayed(projectId: string): string[] {
  try {
    const v = JSON.parse(localStorage.getItem(`${PLAYED_KEY}:${projectId}`) ?? '[]') as unknown;
    return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : [];
  } catch {
    return [];
  }
}

function writePlayed(projectId: string, hrefs: string[]): void {
  try {
    localStorage.setItem(`${PLAYED_KEY}:${projectId}`, JSON.stringify(hrefs));
  } catch {
    /* kept for this page only */
  }
}

/**
 * A workshop project's tour (docs/design/workshops.md), above the session area: the welcome card
 * once, then its levels and stages, the guide card with the stage's text, links and buttons, and
 * Level 1's scoreboard. Kode runs the tour itself; the buttons go to POST /api/workshops/:id/act, and
 * the page itself runs Skip, use my own repo (Open project) and Close tour (top right, and on the
 * end card), which asks first, then closes the tour's project. <full>: no session is open, so the
 * panel takes the room.
 */
export function TourPanel({
  project,
  tour,
  full,
  onOpenProject,
  onError,
}: {
  project: ProjectWithSessions;
  tour: TourState;
  full: boolean;
  /** Opens Open project: the tour stays where it is. */
  onOpenProject: () => void;
  onError: (msg: string) => void;
}) {
  const [busy, setBusy] = useState<string | null>(null);
  const [played, setPlayed] = useState(() => readPlayed(project.id));
  // A level picked on the rail, waiting for Switch: switching archives the open level's sessions.
  const [switchTo, setSwitchTo] = useState<string | null>(null);
  // Close tour, waiting for its confirmation.
  const [closing, setClosing] = useState(false);
  const levelAt = tour.levels.findIndex((l) => l.id === tour.level);
  const level = tour.levels[levelAt];
  // The current stage stays in view in its row, which scrolls sideways on a phone.
  const current = useRef<HTMLLIElement | null>(null);
  useEffect(() => {
    const li = current.current;
    const row = li?.parentElement;
    if (li && row) row.scrollLeft = Math.max(0, li.offsetLeft - row.offsetLeft - (row.clientWidth - li.offsetWidth) / 2);
  }, [tour.level, tour.stage]);
  // The **m** the text names is Meko's mark on a tab: shown in Meko's blue.
  const text = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    for (const b of text.current?.querySelectorAll('strong') ?? []) b.classList.toggle('tour-m', b.textContent === 'm');
  }, [tour.text, tour.lead]);

  const act = async (a: TourAction | { id: 'goto'; label: string }, levelId?: string) => {
    if (a.id === 'own-repo') return onOpenProject();
    if (a.id === 'close') {
      setSwitchTo(null);
      return setClosing(true);
    }
    setBusy(a.id);
    try {
      const r = await api.tourAct(tour.workshop, { stage: tour.stage, action: a.id, ...(levelId ? { level: levelId } : {}) });
      setSwitchTo(null);
      if (r.sessionId) navigate({ view: 'project', projectId: project.id, sessionId: r.sessionId });
    } catch (e) {
      onError(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };

  const closeTour = async () => {
    setBusy('close');
    try {
      await api.closeProject(project.id);
      navigate({ view: 'root' });
    } catch (e) {
      onError(errorMessage(e));
      setBusy(null);
    }
  };

  const play = (l: TourLink) => {
    if (played.includes(l.href)) return;
    const next = [...played, l.href];
    writePlayed(project.id, next);
    setPlayed(next);
  };

  // A link that comes first (Play the game) is the blue button until it's clicked.
  const firsts = tour.links.filter((l) => l.first);
  const others = tour.links.filter((l) => !l.first);
  const waiting = firsts.some((l) => !played.includes(l.href));
  const button = (a: TourAction) => (
    <button key={a.id} className={cx('btn small', a.primary && !waiting && 'primary', a.danger && 'danger')} disabled={!!busy} onClick={() => void act(a)}>
      {busy === a.id ? <Busy label={a.label} /> : a.label}
    </button>
  );
  const link = (l: TourLink) => (
    <a
      key={l.href}
      className={cx('btn small', l.first && !played.includes(l.href) && 'primary')}
      href={l.kind === 'play' ? appUrl(`/${l.href}`) : l.href}
      target="_blank"
      rel="noopener noreferrer"
      onClick={() => l.first && play(l)}
    >
      {l.label}
    </a>
  );
  const target = tour.levels.findIndex((l) => l.id === switchTo) + 1;

  return (
    <section className={cx('tour', full && 'tour-full')} aria-label="Kode tour">
      <div className="tour-head">
        {!tour.welcome && (
          <>
            <div className="tour-levels" role="tablist" aria-label="Levels">
              {tour.levels.map((l, i) => (
                <button
                  key={l.id}
                  role="tab"
                  aria-selected={l.id === tour.level}
                  className={cx('tour-level', l.id === tour.level && 'on', l.finished && 'finished')}
                  title={l.title}
                  onClick={() => {
                    setClosing(false);
                    setSwitchTo(l.id === tour.level && !l.finished ? null : l.id);
                  }}
                >
                  Level {i + 1}
                  {l.finished && <span className="tour-tick" aria-label=", finished"> ✓</span>}
                </button>
              ))}
            </div>
            <ol className="tour-rail" aria-label={`${level?.title ?? 'Level'} stages`}>
              {level?.stages.map((s, i) => (
                <li key={s.id} ref={s.status === 'current' ? current : undefined} className={cx('tour-stage', s.status)} aria-current={s.status === 'current' ? 'step' : undefined}>
                  {i + 1} · {s.title}
                  {s.status === 'skipped' && <span className="sr-only">, skipped</span>}
                </li>
              ))}
            </ol>
          </>
        )}
        <button className="btn small danger tour-close" disabled={!!busy} onClick={() => void act({ id: 'close', label: 'Close tour' })}>
          Close tour
        </button>
      </div>

      {closing && (
        <div className="tour-switch" role="status">
          <span>Close the tour? Kode closes this project and archives its sessions. Start the Kode tour in the command palette takes it again.</span>
          <button className="btn small danger" disabled={!!busy} onClick={() => void closeTour()}>
            {busy === 'close' ? <Busy label="Closing" /> : 'Close tour'}
          </button>
          <button className="btn small" disabled={!!busy} onClick={() => setClosing(false)}>
            Cancel
          </button>
        </div>
      )}

      {switchTo && (
        <div className="tour-switch" role="status">
          <span>
            {tour.levels.find((l) => l.id === switchTo)?.finished ? `Take Level ${target} again?` : `Switch to Level ${target}?`} Kode archives this level's open
            sessions and starts Level {target} from its first stage.
          </span>
          <button className="btn small primary" disabled={!!busy} onClick={() => void act({ id: 'goto', label: 'Switch' }, switchTo)}>
            {busy === 'goto' ? <Busy label="Switching" /> : 'Switch'}
          </button>
          <button className="btn small" onClick={() => setSwitchTo(null)}>
            Cancel
          </button>
        </div>
      )}

      <div className="tour-body">
        <div className="tour-card">
          {tour.welcome ? (
            <h2 className="tour-welcome">{tour.title}</h2>
          ) : (
            <div className="tour-card-head">
              <span className="tour-label">Guiding you</span>
              {level && <span className="tour-level-title">{level.title}</span>}
            </div>
          )}
          <div className="tour-text" ref={text}>
            {tour.lead && (
              <div className="tour-lead">
                <MarkdownView markdown={tour.lead} />
              </div>
            )}
            <MarkdownView markdown={tour.text} />
          </div>
          {tour.phase === 'running' && (
            <div className="tour-running">
              <Busy label="Working" />
            </div>
          )}
          {(tour.actions.length > 0 || tour.links.length > 0) && (
            <div className="tour-actions">
              {firsts.map(link)}
              {tour.actions.map(button)}
              {others.map(link)}
            </div>
          )}
        </div>
        {tour.score && (
          <dl className="tour-score" aria-label="Scoreboard">
            <div>
              <dt>Review without context</dt>
              <dd>{sideText(tour.score.cold)}</dd>
            </div>
            <div>
              <dt>Review with context</dt>
              <dd>{sideText(tour.score.context)}</dd>
            </div>
            <div className="tour-diff">
              <dt>Difference</dt>
              <dd>{tour.score.difference ?? 'Fills in as you go'}</dd>
            </div>
          </dl>
        )}
      </div>
    </section>
  );
}
