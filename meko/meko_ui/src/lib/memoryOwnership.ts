// Whether the signed-in account wrote a memory, which is what gates the
// delete affordance. Shared: any surface listing memories from GET /memories.

import type { Memory } from "@/lib/api/mekoApi";

/**
 * True when `memory` belongs to `accountId`.
 *
 * Prefers the server's `is_mine`, which survives the attribution blanking a
 * viewer's listing gets: deleting a memory is author-scoped, so a member
 * demoted to viewer still owns — and may still delete — what they wrote
 * before, and comparing `user_id` would hide the control on their own rows.
 *
 * Falls back to the id comparison against an api_server that predates
 * `is_mine`, where attribution is present for everyone who can see it.
 */
export function isOwnMemory(memory: Memory, accountId: string | undefined): boolean {
  if (memory.is_mine !== undefined) {
    return memory.is_mine;
  }
  // Case-insensitive, like the uuid comparison the server and the delete's
  // SQL both use: the same id in a different case is the same id, and
  // treating it as a different one hides delete on the caller's own rows.
  const owner = memory.user_id?.trim().toLowerCase();
  const caller = accountId?.trim().toLowerCase();
  return Boolean(owner) && owner === caller;
}
