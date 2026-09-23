// "What is a Collection?" — the explanation above the collection list.
//
// Collapsed by default and expanded on demand, matching "What is a Datapack?"
// on the Datapacks page. It explains a fixed two-level shape, so it is worth
// reading once and in the way after that.
//
// The diagram carries the user's own collection when they have one, because a
// tree with their folder names in it says more than an invented one. The
// fallback is our own default layout, not a made-up workspace.

import { ChevronDown, ChevronUp, Database, Folder, FolderOpen } from "lucide-react";
import { useState } from "react";
import { Card } from "@/components/ui/card";
import type { Collection } from "@/lib/api/collectionsApi";
import { countLabel } from "@/features/collections/collectionsCopy";

/** Shown when the user has no collection worth drawing yet. These are the
 *  folders our own Software delivery layout creates, so the illustration is
 *  something they can actually go and make. */
const SAMPLE = { name: "adlc", folders: ["coding_standards", "features", "integration_tests"] };

/** At most this many folders are drawn; the rest collapse into a count. Keeps
 *  the diagram the same height whether a collection has three folders or thirty. */
const MAX_FOLDERS_DRAWN = 3;

/** The two levels, drawn. A collection holds folders, a folder holds datapacks —
 *  folders do not nest, and the diagram is the quickest way to say so. */
function CollectionShapeDiagram({ example }: { example?: Collection }) {
  const name = example?.collection_name ?? SAMPLE.name;
  const allFolders = (example?.folder_names?.length ? example.folder_names : SAMPLE.folders) ?? [];
  const folders = allFolders.slice(0, MAX_FOLDERS_DRAWN);
  const hidden = allFolders.length - folders.length;

  return (
    <div className="w-full shrink-0 rounded-xl border border-border bg-background/60 p-5 lg:w-72">
      <p className="text-xs uppercase tracking-wide text-muted-foreground">
        {example ? "Your collection" : "Example"}
      </p>

      <div className="mt-3 flex items-center gap-2">
        <Folder className="h-4 w-4 shrink-0 text-brand" aria-hidden />
        <span className="truncate font-mono text-sm font-semibold text-foreground" title={name}>
          {name}
        </span>
      </div>

      {/* The rail is the nesting: everything inside it is one level down. */}
      <ul className="mt-2 space-y-1.5 border-l border-border pl-4">
        {folders.map((folder) => (
          <li key={folder} className="flex items-center gap-2">
            <FolderOpen className="h-3.5 w-3.5 shrink-0 text-muted-foreground" aria-hidden />
            <span className="truncate font-mono text-xs text-foreground/90" title={folder}>
              {folder}
            </span>
          </li>
        ))}
        {hidden > 0 ? (
          <li className="pl-[1.375rem] text-xs text-muted-foreground">
            +{countLabel(hidden, "more folder")}
          </li>
        ) : null}

        <li className="flex items-center gap-2 pt-1">
          <Database className="h-3.5 w-3.5 shrink-0 text-muted-foreground" aria-hidden />
          <span className="text-xs text-muted-foreground">
            {example ? countLabel(example.datapack_count, "datapack") : "datapacks in each folder"}
          </span>
        </li>
      </ul>
    </div>
  );
}

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

          <CollectionShapeDiagram example={example} />
        </div>
      ) : null}
    </Card>
  );
}
