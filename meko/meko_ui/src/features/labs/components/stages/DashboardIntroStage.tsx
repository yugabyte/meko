// Opens Chapter 4 by separating two things the chapter does: Meko already
// keeps enough about your datapack to draw a dashboard, and separately every
// datapack can back its own app with a real Postgres instance. The write
// path from a review into that instance doesn't exist yet, and this page
// says so rather than implying the next stage builds it.

import { LabStageHeader } from "./LabStageHeader";

const POINTS = [
  {
    title: "Meko already knows your activity",
    body: "Every review writes a conversation into this datapack, and token accounting runs on all of it. That's enough for a dashboard without storing anything new.",
  },
  {
    title: "Memory is for facts, not rows",
    body: "Meko's memory store holds discrete facts and preferences. Great for \"remember this\", wrong for \"average cost per model\".",
  },
  {
    title: "Your own runs, not a sample",
    body: "The numbers on the next screen are this datapack's: what you ran, what it extracted, what it saved.",
  },
];

export function DashboardIntroStage() {
  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 4 · Step 1"
        title="Where a dashboard's data comes from"
        lede="Meko already records enough about this datapack to chart it. The next screen reads that back."
      />

      <div className="grid gap-3 sm:grid-cols-3">
        {POINTS.map(({ title, body }) => (
          <div key={title} className="rounded-lg border border-border bg-card p-4">
            <p className="text-sm font-semibold text-foreground">{title}</p>
            <p className="mt-1 text-xs text-muted-foreground">{body}</p>
          </div>
        ))}
      </div>

      <p className="text-xs text-muted-foreground">
        One honest gap: a review's own tokens, cost and findings aren't persisted anywhere yet, so the
        dashboard's per-run table covers this session only. Everything else on it is read back from Meko.
      </p>
    </div>
  );
}
