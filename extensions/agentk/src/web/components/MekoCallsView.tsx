import { useCallback, useEffect, useRef, useState } from 'react';
import { MEKO_LOG_TOOLS, type MekoCallBy, type MekoCallDetail, type MekoCallSummary, type MekoLogInfo, type ProjectWithSessions } from '../../shared/types';
import { api, errorMessage } from '../api';
import { copyText } from '../clipboard';
import { useIsMobile } from '../hooks/useMedia';
import { navigate } from '../router';
import { agentName } from '../../shared/harnesses';
import { cx, liveWithReviews } from '../util';
import { Busy, ConfirmButton } from './common';
import { TopBar } from './TopBar';

/** A call's time, to the second: the date too when it isn't today. */
export function callTime(ms: number, now = Date.now()): string {
  const d = new Date(ms);
  const time = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
  if (d.toDateString() === new Date(now).toDateString()) return time;
  return `${d.toLocaleDateString([], { month: 'short', day: 'numeric' })} ${time}`;
}

/** How long a call took; nothing when its agent doesn't say (Kiro). */
export function callDuration(ms: number | null): string {
  if (ms === null) return '';
  return ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(1)} s`;
}

/** What was sent or came back, for a <pre>: JSON indented, text as it is. */
function shown(v: unknown): string {
  if (v === null || v === undefined) return 'Nothing';
  return typeof v === 'string' ? v : JSON.stringify(v, null, 2);
}

type Opened = { state: 'loading' } | { state: 'error'; error: string } | { state: 'done'; call: MekoCallDetail };


/**
 * The project's Meko call logs (mekolog.ts on the server): one session's at a time, newest first,
 * AgentK's calls and the session's agent's (Claude, Kiro or OpenCode), each call opening to what was sent and what came back. `sessionId`
 * is the session asked for in the route; without one, the session written to last.
 */
export function MekoCallsView({
  project,
  sessionId,
  onMenu,
  onError,
}: {
  project: ProjectWithSessions;
  sessionId: string | null;
  onMenu: () => void;
  onError: (m: string) => void;
}) {
  const mobile = useIsMobile();
  const [logs, setLogs] = useState<MekoLogInfo[] | null>(null);
  const [tool, setTool] = useState('');
  const [by, setBy] = useState<MekoCallBy | ''>('');
  const [failed, setFailed] = useState(false);
  const [calls, setCalls] = useState<MekoCallSummary[] | null>(null);
  const [more, setMore] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [opened, setOpened] = useState<Record<string, Opened>>({});
  const [reload, setReload] = useState(0);
  const [turningOn, setTurningOn] = useState(false);
  // Bumped whenever the list is loaded afresh: a Load older answer for a view that's gone is dropped.
  const view = useRef(0);
  const logging = project.settings.mekoLog;

  useEffect(() => {
    api.mekoLogs(project.id).then(setLogs, (e) => setError(errorMessage(e)));
  }, [project.id, reload]);

  // The session shown: the one asked for, else the one written to last, else the newest running one.
  // A task's code review makes Meko calls of its own (the passes' gotcha searches), so it's listed too.
  const live = liveWithReviews(project);
  const sid = sessionId ?? logs?.[0]?.sessionId ?? (logs ? (live.at(-1)?.id ?? null) : null);
  const choices: { id: string; name: string }[] = (logs ?? []).map((l) => ({ id: l.sessionId, name: l.name }));
  for (const s of live) if (!choices.some((c) => c.id === s.id)) choices.push({ id: s.id, name: s.name });
  if (sid && !choices.some((c) => c.id === sid)) choices.unshift({ id: sid, name: sid });
  const hasLog = !!sid && !!logs?.some((l) => l.sessionId === sid);
  // The session's agent, which made its `claude` calls.
  const agent = agentName({ harness: logs?.find((l) => l.sessionId === sid)?.harness ?? live.find((s) => s.id === sid)?.harness ?? 'claude' });
  const byLabel = (by: MekoCallBy) => (by === 'claude' ? agent : 'AgentK');

  useEffect(() => {
    if (!sid) return;
    let gone = false;
    view.current++;
    setLoading(true);
    setOpened({});
    api
      .mekoCalls(sid, { tool, by, failed })
      .then((p) => {
        if (gone) return;
        setCalls(p.calls);
        setMore(p.more);
        setError(null);
      })
      .catch((e) => !gone && setError(errorMessage(e)))
      .finally(() => !gone && setLoading(false));
    return () => {
      gone = true;
    };
  }, [sid, tool, by, failed, reload]);

  const older = async () => {
    const last = calls?.at(-1);
    if (!sid || !last) return;
    const asked = view.current;
    setLoading(true);
    try {
      const p = await api.mekoCalls(sid, { before: last.offset, tool, by, failed });
      if (view.current !== asked) return;
      setCalls((c) => [...(c ?? []), ...p.calls]);
      setMore(p.more);
    } catch (e) {
      if (view.current === asked) onError(errorMessage(e));
    } finally {
      if (view.current === asked) setLoading(false);
    }
  };

  const toggle = useCallback(
    (c: MekoCallSummary) => {
      if (!sid) return;
      if (opened[c.id]) {
        setOpened(({ [c.id]: _, ...rest }) => rest);
        return;
      }
      setOpened((o) => ({ ...o, [c.id]: { state: 'loading' } }));
      api.mekoCall(sid, c.offset, c.id).then(
        (call) => setOpened((o) => (o[c.id] ? { ...o, [c.id]: { state: 'done', call } } : o)),
        (e) => setOpened((o) => (o[c.id] ? { ...o, [c.id]: { state: 'error', error: errorMessage(e) } } : o)),
      );
    },
    [sid, opened],
  );

  const turnOn = async () => {
    setTurningOn(true);
    try {
      await api.updateProject(project.id, { settings: { mekoLog: true } });
    } catch (e) {
      onError(errorMessage(e));
    } finally {
      setTurningOn(false);
    }
  };

  const back = () => navigate({ view: 'home', projectId: project.id });
  return (
    <div className="home-view">
      {mobile && (
        <TopBar onMenu={onMenu} sub={project.name} title="Meko calls">
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
        {!mobile && <h1>Meko calls</h1>}
        <span className="muted small">What AgentK and {agent} sent to Meko for a session, and what came back. Deleted with the session.</span>
      </header>
      <div className="home-scroll">
        {!logging && (
          <div className="banner info mc-off" role="status">
            <span className="banner-text">Log Meko calls is off for {project.name}, so no new calls are kept.</span>
            <button className="btn small primary" disabled={turningOn} onClick={() => void turnOn()}>
              {turningOn ? <Busy label="Turning on" /> : 'Turn on'}
            </button>
          </div>
        )}
        <section className="box mc-box">
          <div className="mc-bar">
            <select
              id="mc-session"
              className="input mc-session"
              aria-label="Session"
              value={sid ?? ''}
              disabled={!choices.length}
              onChange={(e) => navigate({ view: 'meko', projectId: project.id, sessionId: e.target.value }, { replace: true })}
            >
              {!choices.length && <option value="">No sessions</option>}
              {choices.map((c) => (
                <option key={c.id} value={c.id}>{c.name}</option>
              ))}
            </select>
            <select id="mc-tool" className="input mc-tool" aria-label="Call" value={tool} onChange={(e) => setTool(e.target.value)}>
              <option value="">All calls</option>
              {MEKO_LOG_TOOLS.map((t) => (
                <option key={t} value={t}>{t}</option>
              ))}
            </select>
            <select id="mc-by" className="input mc-by" aria-label="Made by" value={by} onChange={(e) => setBy(e.target.value as MekoCallBy | '')}>
              <option value="">AgentK and {agent}</option>
              <option value="agentk">AgentK</option>
              <option value="claude">{agent}</option>
            </select>
            <label className="check mc-failed">
              <input id="mc-failed" type="checkbox" checked={failed} onChange={(e) => setFailed(e.target.checked)} />
              <span>Failed only</span>
            </label>
            <span className="sp" />
            <button className="btn small" disabled={loading} onClick={() => setReload((n) => n + 1)}>
              Refresh
            </button>
            {sid && hasLog && (
              <a className="btn small" href={api.mekoLogUrl(sid)} download>
                Download
              </a>
            )}
            {sid && hasLog && (
              <ConfirmButton
                label="Clear"
                className="btn small"
                confirmLabel="Clear"
                prompt="Deletes this session's Meko call log."
                danger
                onConfirm={async () => {
                  try {
                    await api.clearMekoLog(sid);
                    setReload((n) => n + 1);
                  } catch (e) {
                    onError(errorMessage(e));
                  }
                }}
              />
            )}
          </div>
          {error && <p className="error-text">{error}</p>}
          {!sid ? (
            logs ? <p className="muted">This project has no running sessions, so there are no Meko calls to show.</p> : <p className="muted"><Busy label="Loading" /></p>
          ) : calls === null ? (
            !error && <p className="muted"><Busy label="Loading" /></p>
          ) : !calls.length ? (
            <p className="muted">
              {tool || by || failed
                ? 'No calls match.'
                : logging
                  ? `No calls yet. Recall at session start, ${agent}'s searches and saves, design uploads and learn steps show up here as they're made.`
                  : 'No calls were kept for this session.'}
            </p>
          ) : (
            <div className="mc-list">
              {calls.map((c) => {
                const o = opened[c.id];
                return (
                  <div key={c.id} className={cx('mc-call', !c.ok && 'bad')}>
                    <button className="mc-row" aria-expanded={!!o} onClick={() => toggle(c)}>
                      <time className="mc-time">{callTime(c.at)}</time>
                      <span className="mc-tool-name">
                        {c.tool}
                        <span className={cx('mc-tag', c.by)}>{byLabel(c.by)}</span>
                        {c.subagent && <span className="mc-tag">subagent</span>}
                        {c.learn && <span className="mc-tag">learn</span>}
                      </span>
                      <span className="mc-ms">{callDuration(c.ms)}</span>
                      <span className={cx('mc-pill', c.ok ? 'ok' : 'bad')}>
                        {c.ok ? 'ok' : 'failed'}
                        {c.attempts > 1 ? ' · retried' : ''}
                      </span>
                    </button>
                    {o && <CallDetail opened={o} summary={c} />}
                  </div>
                );
              })}
              {more && (
                <div className="mc-more">
                  <button className="btn small" disabled={loading} onClick={() => void older()}>
                    {loading ? <Busy label="Loading" /> : 'Load older'}
                  </button>
                </div>
              )}
            </div>
          )}
        </section>
      </div>
    </div>
  );
}

function CallDetail({ opened, summary }: { opened: Opened; summary: MekoCallSummary }) {
  const [copied, setCopied] = useState(false);
  if (opened.state === 'loading') return <div className="mc-detail muted"><Busy label="Loading" /></div>;
  if (opened.state === 'error') return <div className="mc-detail error-text">{opened.error}</div>;
  const c = opened.call;
  const copy = async () => {
    const { offset: _offset, ...call } = c;
    if (await copyText(JSON.stringify(call, null, 2))) {
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    }
  };
  return (
    <div className="mc-detail">
      {summary.error && <div className="mc-error">{summary.error}</div>}
      <div className="mc-label">
        <span className="label">Sent</span>
        <button className="btn small" onClick={() => void copy()}>
          {copied ? 'Copied' : 'Copy call'}
        </button>
      </div>
      <pre className="mc-json">{shown(c.args)}</pre>
      {c.notice && (
        <>
          <div className="mc-label">
            <span className="label">Claude read</span>
          </div>
          <pre className="mc-json">{c.notice}</pre>
        </>
      )}
      <div className="mc-label">
        <span className="label">{c.notice ? 'Got back (the reply Claude Code saved)' : 'Got back'}</span>
      </div>
      <pre className="mc-json">{shown(c.result)}</pre>
    </div>
  );
}
