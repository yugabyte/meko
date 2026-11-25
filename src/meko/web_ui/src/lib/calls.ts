// Helpers for Meko calls from the call log, shared by Usage's recent calls and a turn's trace.

import type { MekoCall } from "./types";

// The tools that report what they found (result counts), which the trace's "found" total sums.
const SEARCHES = new Set(["context_search", "memory_search", "knowledgebase_search"]);

export const isSearch = (c: MekoCall) => SEARCHES.has(c.tool);

// Whether a call only read: the server says so from the tool's definition; failing that, a search.
export const isRead = (c: MekoCall) => c.read_only ?? isSearch(c);

// How long the call log keeps a call (the server's CallRetention).
export const CALL_RETENTION_DAYS = 90;

export const resultCount = (c: MekoCall) => c.memory_n + c.kb_n + c.conversation_n;

// What a call found, or why it failed.
export function callResults(c: MekoCall): string {
  if (!c.ok) return c.error_code;
  return resultCount(c) > 0 ? `${c.memory_n} mem · ${c.kb_n} kb · ${c.conversation_n} conv` : "—";
}
