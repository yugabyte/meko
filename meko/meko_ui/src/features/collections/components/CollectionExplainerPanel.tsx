// "What is a Collection?" — the explanation above the collection list.
//
// Collapsed by default and expanded on demand, matching "What is a Datapack?"
// on the Datapacks page. It explains a fixed two-level shape, so it is worth
// reading once and in the way after that.
//
// The diagram carries the user's own collection when they have one, because a
// tree with their folder names in it says more than an invented one. The
// fallback is our own default layout, not a made-up workspace.

import { ChevronDown, ChevronUp, Folder } from "lucide-react";
import { useState } from "react";
import { Card } from "@/components/ui/card";
import type { Collection } from "@/lib/api/collectionsApi";
import { countLabel } from "@/features/collections/collectionsCopy";
import { FolderTreePreview } from "@/features/collections/components/FolderTreePreview";

/** Shown when the user has no collection worth drawing yet. These are the
 *  folders our own Software delivery layout creates, so the illustration is
 *  something they can actually go and make. */
const SAMPLE = { name: "adlc", folders: ["coding_standards", "features", "integration_tests"] };

export function CollectionExplainerPanel({ example }: { example?: Collection }) {
  const [expanded, setExpanded] = useState(false);

  return (
    <Card className="overflow-hidden border-border bg-gradient-to-br from-brand/10 via-card to-card p-0">
      <button
        type="button"
        onClick={() => setExpanded((open) => !open)}
        aria-expanded={expanded}
        className="flex w-full items-center justify-between gap-3 px-6 py-4 text-left transition-colors hover:bg-foreground/5"
      >
        <span className="flex items-center gap-2 text-lg font-semibold text-foreground">
          <Folder className="h-5 w-5 text-brand" aria-hidden />
          What is a Collection?
        </span>
        {/* Plain icon, not a Button: the whole header row is already the
            control, and a nested <button> would be invalid. */}
        <span className="shrink-0 text-muted-foreground" aria-hidden>
          {expanded ? <ChevronUp className="h-5 w-5" /> : <ChevronDown className="h-5 w-5" />}
        </span>
      </button>

      {expanded ? (
        // pt-4 on top of the header button's own py-4: 16px alone left the
        // first paragraph and the diagram crowding the title.
        <div className="flex flex-col gap-6 px-6 pb-6 pt-4 lg:flex-row lg:items-start lg:justify-between">
          <div className="min-w-0 max-w-2xl space-y-3">
            <p className="text-sm leading-relaxed text-muted-foreground">
              A group of datapacks that serve one outcome, arranged in folders. A collection holds
              folders, a folder holds datapacks, and that is the whole shape: folders do not nest.
              A datapack belongs to at most one collection.
            </p>
            <p className="text-sm leading-relaxed text-muted-foreground">
              <span className="font-medium text-foreground">What folders are for:</span> keeping
              the datapacks that do similar work together, so you can see at a glance which parts
              of the outcome have context behind them and which do not. Datapacks in no collection
              keep working exactly as they do today.
            </p>
          </div>

          <FolderTreePreview
            label={example ? "Your collection" : "Example"}
            name={example?.collection_name ?? SAMPLE.name}
            folders={
              example?.folder_names?.length ? example.folder_names : SAMPLE.folders
            }
            footer={
              example
                ? countLabel(example.datapack_count, "datapack")
                : "datapacks in each folder"
            }
            className="lg:w-72"
          />
        </div>
      ) : null}
    </Card>
  );
}
