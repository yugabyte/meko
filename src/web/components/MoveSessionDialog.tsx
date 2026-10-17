import { useState } from 'react';
import { STAGE_LABELS, type MoveTarget, type Session } from '../../shared/types';
import { defaultTarget, moveBlockedIn, moveEffect, moveError, moveTargets, promptByDefault, stepOf } from '../../shared/stages';
import { api, errorMessage } from '../api';
import { cx, instructionsBlocker, promptBlocker } from '../util';
import { Busy, Dialog, ErrorText, Field, InstructionsField } from './common';

/** What the instructions box says it does, by whether the move sends the new stage's prompt. */
function instructionsHint(stage: MoveTarget, sendsPrompt: boolean): string {
  if (stage === 'review') return 'Sent after the PR prompt, or on their own if AgentK picks up the branch’s open PR.';
  if (sendsPrompt) return `AgentK gets the ${STAGE_LABELS[stage]} instructions; anything you write here goes after them.`;
  return 'Tell AgentK what to do now. Left empty, AgentK isn’t sent anything.';
}

/** Where the dialog starts: a stage clicked in the stage bar. */
export interface MovePreset {
  stage?: MoveTarget;
}

/** Moves a session to another stage. The rules are in src/shared/stages.ts, shared with the server. */
export function MoveSessionDialog({ session, preset, onClose }: { session: Session; preset?: MovePreset; onClose: () => void }) {
  const [stage, setStage] = useState<MoveTarget>(preset?.stage ?? defaultTarget(session.stage));
  const [task, setTask] = useState('');
  const [instructions, setInstructions] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // The new stage's prompt goes when the move's default says so; instructions go whenever they're given.
  const send = promptByDefault(session.stage, stage);
  const asksTask = send && !session.hasTask;
  const types = send || !!instructions.trim();
  const midTurn = session.status === 'working' || session.status === 'starting' || session.busy;
  const blocker =
    moveBlockedIn(session.stage) ??
    moveError(session, stage) ??
    (midTurn ? 'Wait for AgentK to finish its turn.' : null) ??
    (types ? promptBlocker(session) : null) ??
    instructionsBlocker(instructions);

  const move = async () => {
    if (blocker || busy) return;
    setError(null);
    setBusy(true);
    try {
      await api.moveSession(session.id, {
        stage,
        ...(asksTask && task.trim() ? { task: task.trim() } : {}),
        ...(instructions.trim() ? { instructions: instructions.trim() } : {}),
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
            {busy ? <Busy label="Moving" /> : `Move to ${STAGE_LABELS[stage]}`}
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
          <div className="seg" role="group" aria-labelledby="mv-stage-label">
            {moveTargets(session.kind).map((t) => (
              <button
                key={t}
                id={`mv-stage-${t}`}
                type="button"
                className={cx(t === stage && 'on')}
                aria-pressed={t === stage}
                onClick={() => setStage(t)}
              >
                {STAGE_LABELS[t]}
                {t === stepOf(session.stage) ? ' (now)' : ''}
              </button>
            ))}
          </div>
        </div>
        {asksTask && (
          <Field label="Task (optional)" hint="This session has no task yet. AgentK works on this from now on; left empty, it carries on from the conversation.">
            <textarea id="mv-task" className="input" rows={3} value={task} onChange={(e) => setTask(e.target.value)} />
          </Field>
        )}
        <InstructionsField
          id="mv-instructions"
          value={instructions}
          onChange={setInstructions}
          hint={instructionsHint(stage, send)}
          placeholder={stage === 'review' ? 'e.g. Open it as a draft.' : 'e.g. Keep the public API as it is.'}
        />
        <p className="move-effect">{moveBlockedIn(session.stage) ?? moveError(session, stage) ?? moveEffect(session, stage, asksTask && !!task.trim())}</p>
        <ErrorText error={error} />
      </form>
    </Dialog>
  );
}
