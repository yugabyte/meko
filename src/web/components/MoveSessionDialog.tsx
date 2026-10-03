import { useState } from 'react';
import { STAGE_LABELS, type MoveTarget, type Session } from '../../shared/types';
import { defaultTarget, moveBlockedIn, moveEffect, moveError, moveTargets, offersPrompt, promptByDefault, stepOf } from '../../shared/stages';
import { api, errorMessage } from '../api';
import { cx } from '../util';
import { Dialog, ErrorText, Field } from './common';

/** What the new stage's prompt asks AgentK to do, for the checkbox that sends it. */
const PROMPT_TEXT: Record<Exclude<MoveTarget, 'review'>, string> = {
  explore: 'Send AgentK the Explore instructions (investigate the task; no design, PR or merge)',
  design: "Send AgentK the Design instructions (design the change for your approval, without touching the repo)",
  code: "Send AgentK the Code instructions (implement the task and run the checks; don't open a PR yet)",
};

/** Where the dialog starts: a stage clicked in the stage bar. */
export interface MovePreset {
  stage?: MoveTarget;
}

/** Moves a session to another stage. The rules are in src/shared/stages.ts, shared with the server. */
export function MoveSessionDialog({ session, preset, onClose }: { session: Session; preset?: MovePreset; onClose: () => void }) {
  const [stage, setStage] = useState<MoveTarget>(preset?.stage ?? defaultTarget(session.stage));
  // null: the move's default, which changes with the stage picked.
  const [prompt, setPrompt] = useState<boolean | null>(null);
  const [task, setTask] = useState(session.name);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const pickStage = (t: MoveTarget) => {
    setStage(t);
    setPrompt(null);
  };

  const offers = offersPrompt(session.stage, stage);
  const send = offers && (prompt ?? promptByDefault(session.stage, stage));
  const needsTask = send && !session.hasTask;
  const midTurn = session.status === 'working' || session.status === 'starting' || session.busy;
  const blocker =
    moveBlockedIn(session.stage) ??
    moveError(session, stage) ??
    (midTurn ? 'Wait for AgentK to finish its turn.' : null) ??
    (needsTask && !task.trim() ? 'Give AgentK the task.' : null);

  const move = async () => {
    if (blocker || busy) return;
    setError(null);
    setBusy(true);
    try {
      await api.moveSession(session.id, {
        stage,
        ...(offers ? { prompt: send } : {}),
        ...(needsTask ? { task: task.trim() } : {}),
      });
      onClose();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog
      title="Move to another stage"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" disabled={!!blocker || busy} title={blocker ?? undefined} onClick={() => void move()}>
            {busy ? 'Moving…' : `Move to ${STAGE_LABELS[stage]}`}
          </button>
        </>
      }
    >
      <form
        className="stack"
        onSubmit={(e) => {
          e.preventDefault();
          void move();
        }}
      >
        <div className="field">
          <span className="label" id="mv-stage-label">
            Stage
          </span>
          <div className="seg move-stages" role="group" aria-labelledby="mv-stage-label">
            {moveTargets(session.kind).map((t) => (
              <button
                key={t}
                id={`mv-stage-${t}`}
                type="button"
                className={cx(t === stage && 'on')}
                aria-pressed={t === stage}
                onClick={() => pickStage(t)}
              >
                {STAGE_LABELS[t]}
                {t === stepOf(session.stage) ? ' (now)' : ''}
              </button>
            ))}
          </div>
        </div>
        {needsTask && (
          <Field label="Task" hint="This session has no task yet. AgentK works on this from now on.">
            <textarea id="mv-task" className="input" rows={3} value={task} onChange={(e) => setTask(e.target.value)} />
          </Field>
        )}
        {offers && stage !== 'review' && (
          <label className="check">
            <input id="mv-prompt" type="checkbox" checked={send} onChange={(e) => setPrompt(e.target.checked)} />
            <span>{PROMPT_TEXT[stage]}</span>
          </label>
        )}
        <p className="move-effect">{moveBlockedIn(session.stage) ?? moveError(session, stage) ?? moveEffect(session, stage)}</p>
        <ErrorText error={error} />
      </form>
    </Dialog>
  );
}
