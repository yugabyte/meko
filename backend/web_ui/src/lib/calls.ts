// Helpers for Meko calls from the call log, shared by Usage's recent calls and a turn's trace.

import type { MekoCall } from "./types";

// The tools that look things up, as against those that save or change something.
const SEARCHES = new Set(["context_search", "memory_search", "knowledgebase_search"]);

export const isSearch = (c: MekoCall) => SEARCHES.has(c.tool);

export const resultCount = (c: MekoCall) => c.memory_n + c.kb_n + c.conversation_n;

// What a call found, or why it failed.
export function callResults(c: MekoCall): string {
  if (!c.ok) return c.error_code;
  return resultCount(c) > 0 ? `${c.memory_n} mem · ${c.kb_n} kb · ${c.conversation_n} conv` : "—";
}
