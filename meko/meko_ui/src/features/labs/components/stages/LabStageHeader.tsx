// Standard heading block for a lab stage: chapter/step eyebrow, optional
// tags, title and lede. Shared by every stage so they line up vertically and
// a new stage doesn't re-invent its own heading.

import { Badge } from "@/components/ui/badge";

export function LabStageHeader({
  eyebrow,
  title,
  lede,
  tags,
}: {
  /** Where the user is, e.g. "Chapter 1 · Step 3". */
  eyebrow: string;
  title: string;
  lede?: string;
  tags?: string[];
}) {
  return (
    <header className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
          {eyebrow}
        </span>
        {tags?.map((tag) => (
          <Badge key={tag} variant="secondary" className="text-[10px]">
            {tag}
          </Badge>
        ))}
      </div>
      <h2 className="text-xl font-semibold text-foreground">{title}</h2>
      {lede && <p className="text-sm text-muted-foreground">{lede}</p>}
    </header>
  );
}
