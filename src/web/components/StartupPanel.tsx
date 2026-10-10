import { useEffect, useReducer } from 'react';
import type { Session, StartupStep } from '../../shared/types';
import { STAGE_LABELS, STARTUP_STEPS } from '../../shared/types';
import { kindOf } from '../../shared/stages';
import { cx } from '../util';
import { Busy, Check } from './common';

const STAGE_HINT: Partial<Record<Session['stage'], string>> = {
  explore: 'AgentK investigates and experiments in this worktree. Nothing is merged; end the session when you are done.',
  design: 'AgentK reads the code and writes a design for you to approve before anything changes.',
  code: 'AgentK works on the task directly in this worktree.',
  review: 'AgentK picks up review feedback on the open PR.',
};

function secs(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s`;
}

/**
 * Past this on the last steps, Claude may well be running with its hooks not reaching the server:
 * the panel offers to show the terminal under it.
 */
const STUCK_MS = 90_000;

/**
 * Shown over the terminal while a session starts: the steps, where it is, and the stage it opens in.
 * `onShowTerminal` hides it (the session still counts as starting).
 */
export function StartupPanel({ session, onShowTerminal }: { session: Session; onShowTerminal: () => void }) {
  const [, tick] = useReducer((x: number) => x + 1, 0);
  useEffect(() => {
    const t = setInterval(tick, 1000);
    return () => clearInterval(t);
  }, []);

  const st = session.startup;
  const review = kindOf(session.kind) === 'review-pr';
  const steps = STARTUP_STEPS.filter(
    (s) => !(s.step === 'worktree' && st?.resumed) && !(s.step === 'prompt' && st && !st.hasPrompt),
  ).map((s) => (review && s.step === 'worktree' ? { ...s, label: 'Fetching the PR and its design, and preparing the worktree' } : s));
  const current: StartupStep = st?.step ?? 'loading';
  const currentIdx = steps.findIndex((s) => s.step === current);
  const now = Date.now();
  const slow = current === 'loading' && st && now - st.stepSince > 20000;
  const slowWorktree = current === 'worktree' && st && now - st.stepSince > 10000;
  const stuck = (current === 'loading' || current === 'prompt') && st && now - st.stepSince > STUCK_MS;
  const currentLabel = steps[currentIdx]?.label;

  return (
    <div className="startup">
      {/* Only the step is announced; the timers tick every second. */}
      <span className="sr-only" role="status">
        {slow ? `${currentLabel}. This can take up to a minute.` : currentLabel}
      </span>
      <div className="startup-title">
        <Busy label={st?.resumed ? 'Resuming session' : 'Starting session'} />
      </div>
      <ol className="startup-steps">
        {steps.map((s, i) => {
          const state = i < currentIdx ? 'done' : i === currentIdx ? 'current' : 'todo';
          return (
            <li key={s.step} className={cx('startup-step', state)}>
              <span className="startup-mark" aria-hidden="true">
                {state === 'done' && <Check size={10} />}
              </span>
              <span className="startup-label">
                {state === 'current' ? <Busy label={s.label} /> : s.label}
              </span>
              <span className="startup-time" aria-hidden="true">{state === 'current' && st ? secs(now - st.stepSince) : ''}</span>
            </li>
          );
        })}
      </ol>
      {stuck ? (
        <div className="startup-stuck">
          <p className="startup-note">This is taking longer than usual. Claude may already be running: check the terminal.</p>
          <button className="btn small" onClick={onShowTerminal}>
            Show terminal
          </button>
        </div>
      ) : (
        slow && <p className="startup-note">Claude Code loads its plugins, MCP servers and startup hooks first. This can take up to a minute.</p>
      )}
      {slowWorktree && (
        <p className="startup-note">On a big repo this can take a few minutes. You can switch to other sessions meanwhile; this one keeps starting.</p>
      )}
      <p className="startup-stage">
        <span className="stchip">{STAGE_LABELS[session.stage]}</span> {STAGE_HINT[session.stage] ?? `This session is in the ${STAGE_LABELS[session.stage]} stage.`}
      </p>
      {st && <p className="startup-elapsed" aria-hidden="true">{secs(now - st.startedAt)} so far</p>}
    </div>
  );
}
