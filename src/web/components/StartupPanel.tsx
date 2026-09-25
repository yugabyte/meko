import { useEffect, useReducer } from 'react';
import type { Session, StartupStep } from '../../shared/types';
import { STAGE_LABELS, STARTUP_STEPS } from '../../shared/types';
import { cx } from '../util';
import { Dots } from './common';

const STAGE_HINT: Partial<Record<Session['stage'], string>> = {
  design: 'Claude reads the code and writes a design for you to approve before anything changes.',
  code: 'Claude works on the task directly in this worktree.',
  review: 'Claude picks up review feedback on the open PR.',
};

function secs(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s`;
}

/** Shown over the terminal while a session starts: the steps, where it is, and the stage it opens in. */
export function StartupPanel({ session }: { session: Session }) {
  const [, tick] = useReducer((x: number) => x + 1, 0);
  useEffect(() => {
    const t = setInterval(tick, 1000);
    return () => clearInterval(t);
  }, []);

  const st = session.startup;
  const steps = STARTUP_STEPS.filter(
    (s) => !(s.step === 'worktree' && st?.resumed) && !(s.step === 'prompt' && st && !st.hasPrompt),
  );
  const current: StartupStep = st?.step ?? 'loading';
  const currentIdx = steps.findIndex((s) => s.step === current);
  const now = Date.now();
  const slow = current === 'loading' && st && now - st.stepSince > 20000;

  return (
    <div className="startup" role="status" aria-live="polite">
      <div className="startup-title">
        {st?.resumed ? 'Resuming session' : 'Starting session'}
        <Dots />
      </div>
      <ol className="startup-steps">
        {steps.map((s, i) => {
          const state = i < currentIdx ? 'done' : i === currentIdx ? 'current' : 'todo';
          return (
            <li key={s.step} className={cx('startup-step', state)}>
              <span className="startup-mark" aria-hidden="true">
                {state === 'done' ? '✓' : state === 'current' ? '' : ''}
              </span>
              <span className="startup-label">
                {s.label}
                {state === 'current' && <Dots />}
              </span>
              <span className="startup-time">{state === 'current' && st ? secs(now - st.stepSince) : ''}</span>
            </li>
          );
        })}
      </ol>
      {slow && (
        <p className="startup-note">Claude Code loads its plugins, MCP servers and startup hooks first. This can take up to a minute.</p>
      )}
      <p className="startup-stage">
        <span className="stchip">{STAGE_LABELS[session.stage]}</span> {STAGE_HINT[session.stage] ?? `This session is in the ${STAGE_LABELS[session.stage]} stage.`}
      </p>
      {st && <p className="startup-elapsed">{secs(now - st.startedAt)} so far</p>}
    </div>
  );
}
