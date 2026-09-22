// The per-day retrieval series the datapack charts share: one shape, one
// colour mapping, so the line chart, the bar chart and the legend can't drift.
// Shared: any surface plotting hybrid / knowledge / memory query activity.

import { contextColorVar, type ContextConcept } from "@/lib/contextTones";

export type ActivityPoint = { date: string; hybrid: number; kb: number; memory: number };

export type ActivitySeries = {
  key: keyof Omit<ActivityPoint, "date">;
  label: string;
  /** The context concept this series IS, so it inherits that concept's hue
   *  rather than picking its own. A hybrid query searches knowledge AND
   *  memory, which is what a learning is made of — hence purple. */
  concept: ContextConcept;
};

export const ACTIVITY_SERIES: ActivitySeries[] = [
  { key: "hybrid", label: "Hybrid", concept: "learning" },
  { key: "kb", label: "Knowledge", concept: "knowledge" },
  { key: "memory", label: "Memory", concept: "memory" },
];

/** Stroke/fill value for a series, for SVG attributes that take no class. */
export function seriesColor(series: ActivitySeries, alpha?: number): string {
  return contextColorVar(series.concept, alpha);
}

/** Total queries on one day, across every series. */
export function pointTotal(point: ActivityPoint): number {
  return point.hybrid + point.kb + point.memory;
}
