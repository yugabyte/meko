import { useState } from 'react';
import type { Session } from '../../shared/types';
import { api, errorMessage } from '../api';
import { useTouch } from '../hooks/useMedia';
import { instructionsBlocker, openPrGate, openPrReviews } from '../util';
import { Busy, Dialog, ErrorText, InstructionsField, ReviewFields } from './common';
import type { ReviewOverrides } from '../../shared/codereview';
import { inheritedReview, useReviewDefaults } from '../reviewdefaults';

/**
 * Open PR with instructions for AgentK, sent after the PR prompt (`OpenPrRequest`); Review Changes
 * with instructions when the press starts AgentK's code review first, as its Code review fields stand
 * (`openPrReviews`). Mid-turn it
 * queues, as the gate does, and the instructions wait with it. With an Open PR queued already, it
 * opens on the instructions it goes with (`Session.queuedInstructions`), and saving replaces them:
 * an emptied box removes them.
 */
export function OpenPrDialog({ session, projectReview, onClose }: {
  session: Session;
  /** The project's code review settings, what the session's "Default" stands for over the global ones. */
  projectReview: ReviewOverrides;
  onClose: () => void;
}) {
  // On touch, the keyboard comes up only when a field is tapped.
  const touch = useTouch();
  const [instructions, setInstructions] = useState(session.queuedInstructions ?? '');
  // The session's own code review settings: saved before the PR prompt goes, when changed here.
  const [review, setReview] = useState<ReviewOverrides>(session.review ?? {});
  const reviewDefaults = useReviewDefaults();
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const reviews = openPrReviews(session, review, reviewDefaults, projectReview);
  const pr = openPrGate(session, reviews);
  // The PR prompt goes when the turn ends; between turns, a queued Open PR goes now, with this text.
  const waits = !!pr.queues;
  const queued = session.queued === 'open-pr' && waits;
  const blocker =
    (session.stage !== 'code' ? 'Open PR is only available in the Code stage.' : null) ??
    (session.busy ? 'Wait for AgentK to finish.' : null) ??
    pr.reason ??
    instructionsBlocker(instructions);

  const open = async () => {
    if (blocker || busy) return;
    setError(null);
    setBusy(true);
    try {
      // Always sent, blank too: for a queued Open PR, blank removes its instructions.
      if (JSON.stringify(review) !== JSON.stringify(session.review ?? {})) await api.updateSession(session.id, { review });
      await api.openPr(session.id, { instructions: instructions.trim() });
      onClose();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog
      title={queued ? 'Instructions for the queued Open PR' : `${reviews ? 'Review Changes' : 'Open PR'} with instructions`}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" disabled={!!blocker || busy} title={blocker ?? pr.title} onClick={() => void open()}>
            {busy ? <Busy label={queued ? 'Saving' : 'Opening'} /> : queued ? 'Save instructions' : pr.label}
          </button>
        </>
      }
    >
      <form
        className="stack"
        onSubmit={(e) => {
          e.preventDefault();
          void open();
        }}
      >
        <InstructionsField
          id="pr-instructions"
          value={instructions}
          onChange={setInstructions}
          autoFocus={!touch}
          hint={
            queued
              ? 'They go after the PR prompt when AgentK finishes its turn. Empty the box to send none.'
              : waits
                ? reviews
                  ? 'AgentK is mid-turn: its code review starts when the turn ends, and these go with the PR prompt after it.'
                  : 'AgentK is mid-turn: the PR prompt and these go when the turn ends.'
                : reviews
                  ? "Sent after the PR prompt, which goes once AgentK's code review is done and commits, pushes and opens (or updates) the pull request."
                  : 'Sent after the PR prompt, which commits, pushes and opens (or updates) the pull request.'
          }
          placeholder="e.g. Open it as a draft, and target release-2.4."
        />
        <fieldset className="ns-review">
          <legend className="label">Code review</legend>
          <ReviewFields
            id="pr-review"
            scoped
            inherited={inheritedReview(reviewDefaults, projectReview)}
            value={review}
            disabled={busy}
            onChange={setReview}
          />
        </fieldset>
        <ErrorText error={error} />
      </form>
    </Dialog>
  );
}
