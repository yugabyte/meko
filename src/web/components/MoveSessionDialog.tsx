import { useState } from 'react';
import { STAGE_LABELS, type MoveTarget, type Session, type SessionKind } from '../../shared/types';
import { defaultStage, moveBlockedIn, moveEffect, moveError, moveTargets, movesStep, offersPrompt, promptByDefault, stepOf } from '../../shared/stages';
import { api, errorMessage } from '../api';
import { cx } from '../util';
import { Dialog, ErrorText, Field, KindPicker } from './common';

/** What the new stage's prompt asks AgentK to do, for the checkbox that sends it. */
const PROMPT_TEXT: Record<Exclude<MoveTarget, 'review'>, string> = {
  explore: 'Send AgentK the Explore instructions (investigate the task; no design, PR or merge)',
  design: "Send AgentK the Design instructions (design the change for your approval, without touching the repo)",
  code: "Send AgentK the Code instructions (implement the task and run the checks; don't open a PR yet)",
};

/** Where the dialog starts: a stage clicked in the stage bar, or Explore's Open PR…. */
export interface MovePreset {
  kind?: SessionKind;
  stage?: MoveTarget;
}

/** Changes a session's type, its stage, or both. The rules are in src/shared/stages.ts, shared with the server. */
export function MoveSessionDialog({ session, preset, onClose }: { session: Session; preset?: MovePreset; onClose: () => void }) {
  const [kind, setKind] = useState<SessionKind>(preset?.kind ?? session.kind);
  const [stage, setStage] = useState<MoveTarget>(preset?.stage ?? defaultStage(preset?.kind ?? session.kind, session.stage));
  // null: the move's default, which changes with the stage picked.
  const [prompt, setPrompt] = useState<boolean | null>(null);
  const [task, setTask] = useState(session.name);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const pickKind = (k: SessionKind) => {
    setKind(k);
    setStage(defaultStage(k, session.stage));
    setPrompt(null);
  };
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
    moveError(session, { kind, stage }) ??
    (midTurn ? 'Wait for AgentK to finish its turn.' : null) ??
    (needsTask && !task.trim() ? 'Give AgentK the task.' : null);
  const step = movesStep(session.stage, stage);

  const move = async () => {
    if (blocker || busy) return;
    setError(null);
    setBusy(true);
    try {
      await api.moveSession(session.id, {
        kind,
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
      title="Change type or stage"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" disabled={!!blocker || busy} title={blocker ?? undefined} onClick={() => void move()}>
            {busy ? 'Moving…' : step ? `Move to ${STAGE_LABELS[stage]}` : 'Change type'}
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
        <KindPicker id="mv-kind" value={kind} onChange={pickKind} current={session.kind} />
        <div className="field">
          <span className="label" id="mv-stage-label">
            Stage
          </span>
          <div className="seg move-stages" role="group" aria-labelledby="mv-stage-label">
            {moveTargets(kind).map((t) => (
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
        <p className="move-effect">{moveBlockedIn(session.stage) ?? moveError(session, { kind, stage }) ?? moveEffect(session, stage)}</p>
        <ErrorText error={error} />
      </form>
    </Dialog>
  );
}
