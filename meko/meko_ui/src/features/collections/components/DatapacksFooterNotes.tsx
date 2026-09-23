// Notes under the Datapacks table. Each explains a behaviour the table shows
// but cannot justify on its own — why a datapack can't leave a collection, and
// why "Promoted" is worth a column of its own.

import { Card } from "@/components/ui/card";
import { DATAPACKS_FOOTER_NOTES } from "@/features/collections/collectionsCopy";

export function DatapacksFooterNotes() {
  return (
    <div className="grid gap-4 sm:grid-cols-2">
      {DATAPACKS_FOOTER_NOTES.map((note) => (
        <Card key={note.title} className="border-border bg-card/60 p-5">
          <h3 className="text-sm font-semibold text-foreground">{note.title}</h3>
          <p className="mt-2 text-sm leading-relaxed text-muted-foreground">{note.body}</p>
        </Card>
      ))}
    </div>
  );
}
