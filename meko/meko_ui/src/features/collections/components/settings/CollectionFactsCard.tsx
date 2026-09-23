// What this collection is: name, the layout it was stamped from, when, and who
// can reach it. Read-only; every control lives in a sibling card.

import { Card } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { formatDatapackCreatedDateShorthand } from "@/lib/datapackDateFormat";
import type { Collection, CollectionMembers } from "@/lib/api/collectionsApi";
import { useCollectionThemes } from "@/features/collections/hooks/useCollections";
import { countLabel } from "@/features/collections/collectionsCopy";

/** One fact. `mono` is for values that are identifiers the user typed or
 *  chose; a person's name or a count is prose and reads wrong in mono. */
function FactRow({
  label,
  value,
  mono = false,
}: {
  label: string;
  value: string;
  mono?: boolean;
}) {
  return (
    <div className="flex items-baseline justify-between gap-4 py-2.5">
      <dt className="shrink-0 text-sm text-muted-foreground">{label}</dt>
      <dd
        className={cn("min-w-0 truncate text-right text-sm text-foreground", mono && "font-mono")}
      >
        {value}
      </dd>
    </div>
  );
}

export function CollectionFactsCard({
  collection,
  members,
}: {
  collection: Collection;
  members: CollectionMembers;
}) {
  // Cached for the session, so this costs no request here.
  const { data: themes = [] } = useCollectionThemes();
  // The layout's LABEL, not its key, and no version: theme_version is
  // hardcoded "1.0" for every theme and nothing reads it back, so showing it
  // told the user a constant. Falls back to the stored key if the layout has
  // since been removed from the registry.
  const layoutLabel =
    themes.find((t) => t.key === collection.theme_key)?.label ?? collection.theme_key;

  return (
    <Card className="border-border bg-card p-6">
      <h2 className="text-base font-semibold text-foreground">Collection</h2>
      <dl className="mt-3 divide-y divide-border">
        <FactRow label="Name" value={collection.collection_name} mono />
        <FactRow label="Created from" value={layoutLabel} />
        <FactRow label="Created" value={formatDatapackCreatedDateShorthand(collection.created_at)} />
        <FactRow label="Owner" value={collection.owner_name || "You"} />
        <FactRow label="Members" value={countLabel(members.people, "person", "people")} />
        <FactRow label="Agents active" value={countLabel(members.agents, "agent")} />
      </dl>
    </Card>
  );
}
