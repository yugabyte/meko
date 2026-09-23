// The panel beside the tree: what the selected row is, and what it holds.
//
// Four shapes — collection, folder, datapack, learnings — sharing one layout, so
// clicking between them changes the numbers rather than the furniture. Every
// value is derived from the datapack rows already in hand, so selecting costs
// no request.

import { Card } from "@/components/ui/card";
import { useNowMs, formatRelativeTime } from "@/lib/relativeTime";
import type { CollectionDatapackNode, CollectionDetail, CollectionFolder } from "@/lib/api/collectionsApi";
import {
  latestChange,
  nodesInFolder,
  sumNodes,
  type TreeSelection,
} from "@/features/collections/lib/collectionTree";
import {
  countLabel,
  DATAPACK_NODE_SUBTITLE,
  FOLDER_NODE_BODY,
  FOLDER_NODE_SUBTITLE,
  LEARNINGS_NODE_BODY,
  LEARNINGS_NODE_LABEL,
  LEARNINGS_NODE_SUBTITLE,
  COLLECTION_NODE_BODY,
  COLLECTION_NODE_SUBTITLE,
} from "@/features/collections/collectionsCopy";

type Row = { label: string; value: string };

function InspectorBody({
  title,
  subtitle,
  body,
  rows,
}: {
  title: string;
  subtitle: string;
  body: string;
  rows: Row[];
}) {
  return (
    <Card className="border-border bg-card p-6">
      <div className="space-y-1">
        <h2 className="truncate font-mono text-base font-semibold text-foreground" title={title}>
          {title}
        </h2>
        <p className="text-sm text-muted-foreground">{subtitle}</p>
      </div>

      <p className="mt-4 text-sm leading-relaxed text-muted-foreground">{body}</p>

      <dl className="mt-5 divide-y divide-border">
        {rows.map((row) => (
          <div key={row.label} className="flex items-baseline justify-between gap-4 py-2.5">
            <dt className="shrink-0 text-sm text-muted-foreground">{row.label}</dt>
            <dd className="min-w-0 truncate text-right text-sm font-medium text-foreground">
              {row.value}
            </dd>
          </div>
        ))}
      </dl>
    </Card>
  );
}

/** "3 min ago", or a dash when the set is empty. */
function changeLabel(nodes: CollectionDatapackNode[], nowMs: number): string {
  const at = latestChange(nodes);
  return at ? formatRelativeTime(at, nowMs) : "no changes yet";
}

export function CollectionInspector({
  detail,
  selection,
}: {
  detail: CollectionDetail;
  selection: TreeSelection;
}) {
  const nowMs = useNowMs();
  const { collection, folders, datapacks, members } = detail;

  if (selection.kind === "folder") {
    const folder = folders.find((f: CollectionFolder) => f.folder_id === selection.folderId);
    if (!folder) return null;
    const inside = nodesInFolder(datapacks, folder.folder_id);
    const totals = sumNodes(inside);
    return (
      <InspectorBody
        title={folder.folder_name}
        subtitle={FOLDER_NODE_SUBTITLE}
        body={FOLDER_NODE_BODY}
        rows={[
          { label: "Datapacks", value: String(totals.datapacks) },
          { label: "Knowledge files", value: String(totals.knowledge) },
          { label: "Promoted memories", value: String(totals.promoted) },
          { label: "Private memories", value: String(totals.memories) },
          { label: "Last change", value: changeLabel(inside, nowMs) },
        ]}
      />
    );
  }

  if (selection.kind === "datapack") {
    const node = datapacks.find((d) => d.datapack_id === selection.datapackId);
    if (!node) return null;
    const folder = folders.find((f) => f.folder_id === node.folder_id);
    return (
      <InspectorBody
        title={folder ? `${folder.folder_name}/${node.datapack_name}` : node.datapack_name}
        subtitle={
          node.created_by_agent_id
            ? `${DATAPACK_NODE_SUBTITLE} · created by ${node.created_by_agent_id}`
            : DATAPACK_NODE_SUBTITLE
        }
        body={`Everything one piece of work knows. Owned by ${node.datapack_owner_name || "you"}.`}
        rows={[
          { label: "Private memories", value: String(node.memory_count) },
          { label: "Promoted memories", value: String(node.promoted_count) },
          { label: "Awaiting review", value: String(node.awaiting_review_count) },
          { label: "Knowledge files", value: String(node.knowledge_count) },
          { label: "Last change", value: formatRelativeTime(node.updated_at, nowMs) },
        ]}
      />
    );
  }

  if (selection.kind === "learnings") {
    const totals = sumNodes(datapacks);
    return (
      <InspectorBody
        title={LEARNINGS_NODE_LABEL}
        subtitle={LEARNINGS_NODE_SUBTITLE}
        body={LEARNINGS_NODE_BODY}
        rows={[
          { label: "Private memories", value: String(totals.memories) },
          { label: "Promoted memories", value: String(totals.promoted) },
          { label: "Awaiting review", value: String(totals.awaitingReview) },
          { label: "Across", value: countLabel(totals.datapacks, "datapack") },
        ]}
      />
    );
  }

  const totals = sumNodes(datapacks);
  return (
    <InspectorBody
      title={collection.collection_name}
      subtitle={COLLECTION_NODE_SUBTITLE}
      body={COLLECTION_NODE_BODY}
      rows={[
        { label: "Datapacks", value: String(totals.datapacks) },
        { label: "Folders", value: String(folders.length) },
        { label: "Members", value: countLabel(members.people, "person", "people") },
        // Separate row rather than "3 people · 2 agents": one counts who may
        // read, the other who has written, and one line invites them to be
        // read as the same population.
        { label: "Agents active", value: countLabel(members.agents, "agent") },
        { label: "Owner", value: collection.owner_name || "You" },
      ]}
    />
  );
}
