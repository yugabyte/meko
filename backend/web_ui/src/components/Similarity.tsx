// A cosine similarity as a colored badge, and the floor below which a memory isn't a match: Search
// hides memories under it, and the memory map leaves them unlit.

import { Badge } from "./ui";

// context_search returns every memory it ranks, as Cloud's does, for an agent to weigh beside the
// other sources; a person reading the list needs only the matches. Measured with Meko's embedder,
// right answers scored 0.40 to 0.72 and unrelated memories 0.11 to 0.29.
export const WEAK_MEMORY = 0.4;

// Rounded to the two places the badge shows, so a memory shown as 0.40 counts as 0.40.
export const round2 = (v: number) => Math.round(v * 100) / 100;

export function Similarity({ value }: { value: number | undefined }) {
  if (value === undefined) return null;
  return (
    <Badge tone={value >= 0.5 ? "success" : value >= WEAK_MEMORY ? "primary" : "neutral"}>{value.toFixed(2)}</Badge>
  );
}
