// What the current selection holds, in numbers.
//
// Four shapes — project, folder, datapack, learnings — sharing one frame,
// so moving between them changes the values rather than the furniture. Every
// number is summed from the datapack rows the project response already
// carried, so selecting costs no request.
//
// The header repeats the selected row's icon and name on purpose: it is what
// tells you this pane belongs to that row rather than being a second,
// unrelated panel.

import { Database, Folder, FolderTree, Sparkles } from "lucide-react";
import { TruncatedText } from "@/components/ui/truncated-text";
import { cn } from "@/lib/utils";
import { useNowMs, formatRelativeTime } from "@/lib/relativeTime";
import type { ProjectDatapackNode, ProjectDetail } from "@/lib/api/projectsApi";
import {
  latestChange,
  nodesInFolder,
  sumNodes,
  type TreeSelection,
} from "@/features/projects/lib/projectTree";
import {
  countLabel,
  DATAPACK_NODE_SUBTITLE,
  FOLDER_NODE_BODY,
  FOLDER_NODE_SUBTITLE,
  LEARNINGS_NODE_BODY,
  LEARNINGS_NODE_LABEL,
  LEARNINGS_NODE_SUBTITLE,
  PROJECT_NODE_BODY,
  PROJECT_NODE_SUBTITLE,
  KB_FILES_LABEL,
} from "@/features/projects/projectsCopy";
import { DATAPACK_NAME_FONT, LABEL_NAME_FONT } from "@/features/projects/projectsTypography";

type Row = { label: string; value: string; hint?: string };

export function SelectionDetailPane({
  detail,
  selection,
  className,
}: {
  detail: ProjectDetail;
  selection: TreeSelection;
  className?: string;
}) {
  const nowMs = useNowMs();
  const { project, folders, datapacks, members } = detail;

  let icon: React.ReactNode = <FolderTree className="h-4 w-4 text-brand" aria-hidden />;
  let title = project.project_name;
  let subtitle = PROJECT_NODE_SUBTITLE;
  let body = PROJECT_NODE_BODY;
  let rows: Row[] = [];
  let mono = false;

  if (selection.kind === "project") {
    const t = sumNodes(datapacks);
    rows = [
      { label: "Datapacks", value: String(t.datapacks) },
      { label: "Folders", value: String(folders.length) },
      { label: "Members", value: countLabel(members.people, "person", "people") },
      { label: "Agents active", value: countLabel(members.agents, "agent") },
      { label: "Owner", value: project.owner_name || "You" },
    ];
  } else if (selection.kind === "folder") {
    const folder = folders.find((f) => f.folder_id === selection.folderId);
    const inFolder = nodesInFolder(datapacks, selection.folderId);
    const t = sumNodes(inFolder);
    const last = latestChange(inFolder);
    icon = <Folder className="h-4 w-4 text-brand" aria-hidden />;
    title = folder?.folder_name ?? "Folder";
    subtitle = FOLDER_NODE_SUBTITLE;
    body = folder?.folder_description?.trim() || FOLDER_NODE_BODY;
    rows = [
      { label: "Datapacks", value: String(t.datapacks) },
      { label: "Memories", value: String(t.memories) },
      { label: "Promoted", value: String(t.promoted) },
      { label: KB_FILES_LABEL, value: String(t.knowledge) },
      { label: "Last change", value: last ? formatRelativeTime(last, nowMs) : "—" },
    ];
  } else if (selection.kind === "datapack") {
    const node = datapacks.find((d) => d.datapack_id === selection.datapackId);
    icon = <Database className="h-4 w-4 text-brand" aria-hidden />;
    title = node?.datapack_name ?? "Datapack";
    subtitle = DATAPACK_NODE_SUBTITLE;
    body = "";
    rows = node
      ? [
          // Private first because it is the larger number. The two are
          // DISJOINT, not nested: promotion moves a memory into
          // promoted_collection and deletes the mem0 row, and the rollup
          // counts them from the two separate tables.
          { label: "Private memories", value: String(node.memory_count) },
          { label: "Awaiting review", value: String(node.awaiting_review_count) },
          { label: "Promoted", value: String(node.promoted_count) },
          { label: KB_FILES_LABEL, value: String(node.knowledge_count) },
          { label: "Owner", value: node.datapack_owner_name || "You" },
          { label: "Last change", value: formatRelativeTime(node.updated_at, nowMs) },
        ]
      : [];
    mono = true;
  } else {
    const t = sumNodes(datapacks);
    icon = <Sparkles className="h-4 w-4 text-brand" aria-hidden />;
    title = LEARNINGS_NODE_LABEL;
    subtitle = LEARNINGS_NODE_SUBTITLE;
    body = LEARNINGS_NODE_BODY;
    mono = false;
    rows = [
      { label: "Memories", value: String(t.memories) },
      { label: "Awaiting review", value: String(t.awaitingReview) },
      { label: "Promoted", value: String(t.promoted) },
    ];
  }

  return (
    <div
      className={cn(
        // The brand edge is the same one the selected row carries, which is
        // what visually joins this pane to it.
        "flex min-h-0 flex-col rounded-xl border border-border border-l-2 border-l-brand bg-card",
        className,
      )}
    >
      <div className="border-b border-border px-4 py-3">
        <div className="flex items-center gap-2">
          {icon}
          <TruncatedText
            text={title}
            side="bottom"
            className={cn(
              "text-sm font-semibold text-foreground",
              mono ? DATAPACK_NAME_FONT : LABEL_NAME_FONT,
            )}
          />
        </div>
        <p className="mt-0.5 text-xs text-muted-foreground">{subtitle}</p>
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto px-4 py-3">
        {body ? <p className="mb-3 text-sm leading-relaxed text-muted-foreground">{body}</p> : null}
        <dl className="divide-y divide-border">
          {rows.map((row) => (
            <div key={row.label} className="flex items-baseline justify-between gap-4 py-2.5">
              <dt className="shrink-0 text-sm text-muted-foreground">
                {row.label}
                {row.hint ? (
                  <span className="block text-xs text-muted-foreground/70">{row.hint}</span>
                ) : null}
              </dt>
              <dd className="min-w-0 truncate text-right text-sm font-medium text-foreground">
                {row.value}
              </dd>
            </div>
          ))}
        </dl>
      </div>
    </div>
  );
}
