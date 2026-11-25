// Where a work item's ⋯ move puts it, shared by the server and the page (which shows the move before the server has it).
import type { WorkMove } from './types.js';

/** The queue's ids with `id` moved one place up or down, or to the top or bottom; unchanged when `id` isn't in them. */
export function moveItem(ids: string[], id: string, to: WorkMove): string[] {
  const at = ids.indexOf(id);
  if (at < 0) return ids;
  const dest = to === 'top' ? 0 : to === 'bottom' ? ids.length - 1 : to === 'up' ? Math.max(0, at - 1) : Math.min(ids.length - 1, at + 1);
  const out = ids.filter((x) => x !== id);
  out.splice(dest, 0, id);
  return out;
}
