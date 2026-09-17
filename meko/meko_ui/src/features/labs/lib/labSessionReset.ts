// Wipes everything a lab run persisted, so Exit and Restart both hand the
// next visitor an untouched lab.
//
// The keys live in two hooks (useLabProgress writes position + completion,
// useLabRunnerSession writes the datapack, deployment and lesson-trace
// choices) and are listed here rather than exported from each, because the
// thing that has to stay true is that this list is COMPLETE — a key added
// there and missed here leaves a restarted lab resuming half its old state.

import { removeLocalItem } from "@/lib/browserStorage";

/** Per-lab keys the reset clears, in the `meko:<name>:<labId>` shape both
 *  hooks use.
 *
 *  DELIBERATELY ABSENT: `meko:lab-deployment`, `meko:lab-deployment-mode`,
 *  `meko:lab-amp-cluster` and `meko:lab-amp-branch`. Those hold ids of real
 *  resources running in the user's own account, and the stored id is the only
 *  handle on them -- deploymentsApi offers getDeployment(id) and
 *  getSharedDeployment(slug) and no list endpoint, so nothing can re-derive
 *  one. Forgetting the id would leave a billable instance running invisibly
 *  and walk the user through standing up a second one. Clearing progress does
 *  not need to cost money. */
const EXACT_KEY_PREFIXES = ["meko:lab-progress", "meko:lab-completed", "meko:lab-datapack"];

/** Lesson traces carry a trailing datapack id (`…:<labId>:<datapackId>`), so
 *  there is one per datapack the user touched and no single key to remove. */
const TRACE_KEY_PREFIX = "meko:lab-lesson-trace";

/**
 * Clears this lab's saved progress, completion and datapack choice, and every
 * lesson trace it recorded. Leaves the agent deployment and any AMP cluster
 * alone -- see EXACT_KEY_PREFIXES.
 *
 * Deliberately scoped to one lab id: another lab's run is not this one's to
 * discard. Storage access is best-effort throughout (private mode, denied
 * storage), matching browserStorage's own contract — a reset that cannot
 * write is still better than one that throws mid-way.
 */
export function resetLabState(labId: string): void {
  for (const prefix of EXACT_KEY_PREFIXES) removeLocalItem(`${prefix}:${labId}`);

  // Enumerating is the only way to reach the datapack-keyed trace entries.
  // Collected first, then removed: removing during iteration reindexes
  // localStorage underneath the loop and silently skips entries.
  const traceKeys: string[] = [];
  try {
    for (let i = 0; i < localStorage.length; i += 1) {
      const key = localStorage.key(i);
      if (key?.startsWith(`${TRACE_KEY_PREFIX}:${labId}:`)) traceKeys.push(key);
    }
  } catch {
    // Storage unavailable; the exact keys above were best-effort too.
  }
  for (const key of traceKeys) removeLocalItem(key);
}
