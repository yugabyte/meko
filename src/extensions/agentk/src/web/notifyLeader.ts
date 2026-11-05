// Which open AgentK page shows browser notifications.
//
// Every open page gets each notify event. If each one showed it, the notifications would share a
// tag, only the last page's would survive, and clicking it would focus and navigate that page (a
// forgotten background tab, say) instead of the window the person is using. So one page shows
// them: the one used last. Pages queue for a Web Lock and a page that gets focus steals it; when
// the holder closes, the next page in the queue takes over, so notifications never stop.

const LOCK = 'ak-notifications';

type Locks = Pick<LockManager, 'request'>;

export interface NotifyLeader {
  /** Whether this page should show notifications. */
  readonly leader: boolean;
  /** This page was just used (it got focus): it takes over notifications. */
  claim(): void;
}

export function notifyLeader(locks: Locks | undefined): NotifyLeader {
  // Without Web Locks every page notifies, as before.
  if (!locks) return { leader: true, claim: () => undefined };
  let leader = false;
  // A queued (non-stealing) request is outstanding: never queue a second one.
  let waiting = false;
  const hold = (steal: boolean) => {
    if (!steal) {
      if (waiting) return;
      waiting = true;
    }
    locks
      .request(LOCK, { steal }, () => {
        if (!steal) waiting = false;
        leader = true;
        return new Promise<void>(() => undefined); // held until this page closes or another steals it
      })
      .catch(() => {
        // Stolen by a page the person just used: wait for the lock to come back.
        leader = false;
        hold(false);
      });
  };
  hold(false);
  return {
    get leader() {
      return leader;
    },
    claim() {
      if (!leader) hold(true);
    },
  };
}
