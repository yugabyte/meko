// The three sources Meko builds context from -- shared between
// ContextIntroStage (which introduces all three up front) and
// ContextSourceTracker (which each of the three hands-on stages renders so
// the intro's framing visibly continues into them, instead of each reading
// as an unrelated new page). Order here is the fixed lesson order.

import { FileText, MessageSquare, Sparkles, type LucideIcon } from "lucide-react";

export type ContextSourceKey = "knowledge-base" | "memory" | "conversation";

export const CONTEXT_SOURCES: { key: ContextSourceKey; icon: LucideIcon; title: string; body: string }[] = [
  {
    key: "knowledge-base",
    icon: FileText,
    title: "Knowledge Base",
    body: "Documents you upload — org standards, docs, references. Meko chunks and indexes them so they're searchable.",
  },
  {
    key: "memory",
    icon: Sparkles,
    title: "Memory",
    body: "Individual facts and preferences, either added directly or extracted from a conversation.",
  },
  {
    key: "conversation",
    icon: MessageSquare,
    title: "Conversation",
    body: "Talking with Meko normally — it reads what you say and remembers what matters, without you doing anything extra.",
  },
];
