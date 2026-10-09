// One hue per context concept, for every surface that shows more than one of
// them: the datapack pipeline, the metrics legends and bars, and the charts.
// Shared: anything rendering a knowledge / memory / conversation / learning /
// collective affordance. Values resolve from the `--context-*` tokens in
// index.css, so both modes come for free.

/** The five things Meko stores context as. Ordered as the pipeline reads:
 *  a conversation yields memories, a memory is promoted to a learning, and
 *  a learning the team accepts becomes collective. */
export type ContextConcept = "conversation" | "memory" | "learning" | "knowledge" | "collective";

/** Tailwind utilities for a concept's marker/fill, soft background and text.
 *  Class names are written out in full rather than interpolated — Tailwind
 *  scans source text, so `bg-context-${c}` would be purged. */
type ContextToneClasses = {
  /** Solid fill: legend dots, bar segments, underlines. */
  fill: string;
  /** Tinted background for banners and pills. */
  soft: string;
  /** Saturated text, for a label that carries the concept's colour. */
  text: string;
  /** Border matching `soft`, for a bordered banner. */
  border: string;
};

export const CONTEXT_TONE: Record<ContextConcept, ContextToneClasses> = {
  conversation: {
    fill: "bg-context-conversation",
    soft: "bg-context-conversation-soft",
    text: "text-context-conversation",
    border: "border-context-conversation/30",
  },
  memory: {
    fill: "bg-context-memory",
    soft: "bg-context-memory-soft",
    text: "text-context-memory",
    border: "border-context-memory/30",
  },
  learning: {
    fill: "bg-context-learning",
    soft: "bg-context-learning-soft",
    text: "text-context-learning",
    border: "border-context-learning/30",
  },
  knowledge: {
    fill: "bg-context-knowledge",
    soft: "bg-context-knowledge-soft",
    text: "text-context-knowledge",
    border: "border-context-knowledge/30",
  },
  collective: {
    fill: "bg-context-collective",
    soft: "bg-context-collective-soft",
    text: "text-context-collective",
    border: "border-context-collective/30",
  },
};

/** The same colour as a CSS value, for SVG `fill`/`stroke`, which take no
 *  utility class. `alpha` returns the translucent form an area fill under a
 *  line wants. */
export function contextColorVar(concept: ContextConcept, alpha?: number): string {
  const v = `var(--context-${concept})`;
  return alpha == null ? `hsl(${v})` : `hsl(${v} / ${alpha})`;
}

/** Human labels, so a legend and a tooltip can't disagree about what to call
 *  a series. */
export const CONTEXT_LABEL: Record<ContextConcept, string> = {
  conversation: "Conversations",
  memory: "Memory",
  learning: "Learnings",
  knowledge: "Knowledge Base",
  collective: "Collective",
};

/** Retrieval-quality bands. Not context concepts — these are judgements about
 *  a result, so they keep the app's success/warning/destructive semantics
 *  rather than borrowing a concept's hue. */
export type QualityBand = "high" | "mid" | "low";

export const QUALITY_TONE: Record<QualityBand, { fill: string; text: string }> = {
  high: { fill: "bg-success", text: "text-success" },
  mid: { fill: "bg-warning", text: "text-warning" },
  low: { fill: "bg-destructive", text: "text-destructive" },
};

export function qualityColorVar(band: QualityBand): string {
  const token = band === "high" ? "--success" : band === "mid" ? "--warning" : "--destructive";
  return `hsl(var(${token}))`;
}
