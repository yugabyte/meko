// The whole lab at a glance on its detail page: one card per chapter, in
// order, so the shape of the run is visible before committing to it.

import type { LabChapter } from "../types";

export function LabChapterArc({ chapters }: { chapters: LabChapter[] }) {
  return (
    // auto-fit rather than a fixed column count: the lab has four chapters
    // today and the row shouldn't wrap 3+1, but a fifth shouldn't need this
    // class changed either.
    <ol className="grid grid-cols-[repeat(auto-fit,minmax(14rem,1fr))] gap-3">
      {chapters.map((chapter, index) => (
        <li key={chapter.id} className="flex flex-col gap-1.5 rounded-lg border border-border bg-card p-4">
          <span className="text-xs font-semibold uppercase tracking-wide text-primary">
            Ch {index + 1}
          </span>
          <p className="text-sm font-semibold text-foreground">{chapter.title}</p>
          <p className="text-xs text-muted-foreground">{chapter.summary}</p>
          <p className="mt-auto pt-2 text-xs text-muted-foreground">
            {chapter.stages.length} step{chapter.stages.length === 1 ? "" : "s"}
          </p>
        </li>
      ))}
    </ol>
  );
}
