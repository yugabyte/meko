import type { Session, SessionAction } from '../../shared/types';
import { cx } from '../util';
import { Busy, ConfirmButton } from './common';

/** "3 new commits", or what a force-push means for the review. */
export function newCommitsText(n: number | null): string {
  if (n === null) return 'PR force-pushed';
  return n === 1 ? '1 new commit' : `${n} new commits`;
}

/**
 * A Review a PR session's gate: where the PR stands, Update to latest when it has moved on (or has a
 * design AgentK hasn't read), and Close review.
 */
export function ReviewPrGate({
  session,
  reason,
  blocked,
  midTurn,
  updating,
  onAct,
}: {
  session: Session;
  /** Why Update to latest can't be pressed now, if it can't. */
  reason: string | null;
  /** The session is busy or another action is in flight. */
  blocked: boolean;
  midTurn: boolean;
  /** Update to latest is in flight: fetching the PR can take a while. */
  updating: boolean;
  onAct: (a: SessionAction) => Promise<void>;
}) {
  const rv = session.reviewingPr;
  const gone = !!rv && rv.prState !== 'OPEN';
  const moved = !!rv && !gone && rv.headSha !== rv.reviewSha;
  // A version of the PR's design AgentK hasn't read: Update to latest has it read, commits or not.
  const designUnread = !!rv && !gone && rv.designUnread;
  return (
    <span className="gate-group">
      <span className={cx('pr-pill', (moved || designUnread) && 'moved', gone && 'gone')}>
        {gone ? (rv!.prState === 'MERGED' ? '✓ merged' : 'closed') : moved ? newCommitsText(rv!.newCommits) : designUnread ? 'design updated' : 'up to date'}
      </span>
      {(moved || designUnread) && (
        <button
          className="gate"
          disabled={blocked || !!reason}
          title={reason ?? (moved ? "Check out the PR's latest commits; AgentK sums up what changed" : "AgentK reads the PR's new design and sums it up")}
          onClick={() => onAct('update-pr')}
        >
          {updating ? <Busy label="Updating" /> : 'Update to latest'}
        </button>
      )}
      <ConfirmButton
        className={moved || designUnread ? 'btn small' : 'gate'}
        label="Close review"
        confirmLabel="Close review"
        prompt="The session is archived and its review branch deleted."
        onConfirm={() => onAct('close-review')}
        disabled={blocked || midTurn}
      />
    </span>
  );
}
