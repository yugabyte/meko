// Settings for one collection: what it is, and the two deletes that cannot be
// undone. Layout only; each card owns its own state and dialogs.
//
// No folder operations here. Add and Rename live in the page header beside
// the tree they act on, and moving a datapack is reachable from the Datapacks
// grid and from the datapack's own page. A third copy was just a second place
// to keep in step.
//
// Everything here is owner-only, which the caller enforces by not rendering
// the tab at all for anyone else. Nothing below re-checks it.

import type { Datapack } from "@/lib/api/mekoApi";
import type { CollectionDetail } from "@/lib/api/collectionsApi";
import { CollectionFactsCard } from "@/features/collections/components/settings/CollectionFactsCard";
import { CollectionDangerZoneCard } from "@/features/collections/components/settings/CollectionDangerZoneCard";

export function CollectionSettingsTab({
  detail,
  ownDatapacks,
  onCollectionDeleted,
}: {
  detail: CollectionDetail;
  /** Every datapack the signed-in user owns, for the move and delete pickers. */
  ownDatapacks: Datapack[];
  onCollectionDeleted: () => void;
}) {
  const { collection, datapacks, members } = detail;

  return (
    <div className="grid gap-6 lg:grid-cols-2">
      <CollectionFactsCard collection={collection} members={members} />

      <div className="space-y-6">
        <CollectionDangerZoneCard
          collection={collection}
          datapacks={datapacks}
          ownDatapacks={ownDatapacks}
          onCollectionDeleted={onCollectionDeleted}
        />
      </div>
    </div>
  );
}
