// Datapacks that belong to no collection, listed at the foot of the Collections
// page.
//
// Laid out as header / explanation / chips rather than two columns: the chip
// row needs the full width to stay scannable once a workspace has dozens of
// unfiled datapacks, and the action belongs with the heading where every other
// page puts it.

import { Card } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import type { Datapack } from "@/lib/api/mekoApi";
import {
  FILE_INTO_COLLECTION_LABEL,
  UNFILED_PANEL_BODY,
  UNFILED_PANEL_TITLE,
} from "@/features/collections/collectionsCopy";

/** Roughly one full-width row of chips. Past this the panel stops being a
 *  summary and starts being the list, which is what the Datapacks table is
 *  for — so the remainder collapses into a count. */
const MAX_VISIBLE_TAGS = 6;

export function UnfiledDatapacksPanel({
  datapacks,
  onFileInto,
}: {
  datapacks: Datapack[];
  /** Sends the user to the Datapacks table, where filing happens per row. */
  onFileInto: () => void;
}) {
  if (datapacks.length === 0) return null;

  const visible = datapacks.slice(0, MAX_VISIBLE_TAGS);
  const hidden = datapacks.length - visible.length;

  return (
    <Card className="space-y-4 border-border bg-card p-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 className="text-sm font-semibold text-foreground">
          {UNFILED_PANEL_TITLE}{" "}
          <span className="tabular-nums text-muted-foreground">{datapacks.length}</span>
        </h2>
        {/* `secondary` rather than `outline`: an outline button is the same
            chrome as the Badge chips below and stops reading as an action. */}
        <Button type="button" variant="secondary" size="sm" onClick={onFileInto}>
          {FILE_INTO_COLLECTION_LABEL}
        </Button>
      </div>

      <p className="max-w-3xl text-sm leading-relaxed text-muted-foreground">
        {UNFILED_PANEL_BODY}
      </p>

      <div className="flex flex-wrap items-center gap-1.5">
        {visible.map((dp) => (
          <Badge key={dp.datapack_id} variant="outline" className="font-mono font-normal">
            {dp.datapack_name}
          </Badge>
        ))}
        {hidden > 0 ? (
          <span className="text-xs text-muted-foreground">+{hidden} more</span>
        ) : null}
      </div>
    </Card>
  );
}
