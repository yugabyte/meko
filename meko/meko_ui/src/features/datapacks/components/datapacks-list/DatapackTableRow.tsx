// One row of the Datapacks table: where the datapack lives, its counts, and
// the actions that fit its state. Filed rows open or move; unfiled rows file.
//
// Moving is offered because filing is not final: placement is an upsert keyed
// on the datapack, so a datapack can change folder or change collection as often
// as you like.

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { TableCell, TableRow } from "@/components/ui/table";
import { cn } from "@/lib/utils";
import { formatRelativeTime } from "@/lib/relativeTime";
import type { CurrentUser, Datapack } from "@/lib/api/mekoApi";
import type { DatapackPath } from "@/lib/api/collectionsApi";
import {
  FILE_INTO_COLLECTION_LABEL,
  MOVE_INTO_COLLECTION_LABEL,
  UNFILED_LABEL,
} from "@/features/collections/collectionsCopy";
import {
  datapackAccessFooterLabel,
  datapackTileOwnerLabel,
} from "@/features/datapacks/components/datapacks-list/datapackTileLabels";

export function DatapackTableRow({
  dp,
  path,
  currentUser,
  nowMs,
  onOpenDatapack,
  onFileDatapack,
}: {
  dp: Datapack;
  /** Undefined means unfiled in the caller's tree. */
  path?: DatapackPath;
  currentUser: CurrentUser | null;
  nowMs: number;
  onOpenDatapack: (datapack: Datapack) => void;
  /** Opens the file/move dialog. Absent when the user owns no collection to
   *  file into, in which case a filed row just opens. */
  onFileDatapack?: (datapack: Datapack) => void;
}) {
  const owner = datapackTileOwnerLabel(dp, currentUser);
  const isOwned = owner === "Me";

  return (
    <TableRow>
      {/* text-sm font-medium, matching every other table in the app (see
          admin/UsersPage). Deliberately NOT font-mono: the tiles use mono for
          a name that is the card's headline, but inside a table it reads as a
          different size from the cells beside it. */}
      <TableCell className="text-sm font-medium text-foreground">{dp.datapack_name}</TableCell>

      {/* Collection and folder in one cell. They were two columns, but the folder
          is only meaningful inside its collection, and the unfiled case left a
          whole column reading "UUID only". */}
      <TableCell>
        {path ? (
          <div className="flex min-w-0 flex-col gap-0.5">
            <Badge variant="outline" className="w-fit font-normal">
              {path.collection_name}
            </Badge>
            <span className="truncate text-xs text-muted-foreground" title={path.path}>
              {path.folder_name}
            </span>
          </div>
        ) : (
          <Badge variant="outline" className="font-normal text-muted-foreground">
            {UNFILED_LABEL.toLowerCase()}
          </Badge>
        )}
      </TableCell>

      <TableCell className="tabular-nums text-muted-foreground">{dp.memory_count ?? 0}</TableCell>

      <TableCell
        className={cn(
          "tabular-nums",
          (dp.collective_memory_count ?? 0) > 0
            ? "text-emerald-600 dark:text-emerald-400"
            : "text-muted-foreground",
        )}
      >
        {dp.collective_memory_count ?? 0}
      </TableCell>

      <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
        {isOwned ? "" : `${owner} · `}
        {formatRelativeTime(dp.updated_at, nowMs)}
      </TableCell>

      <TableCell className="text-xs text-muted-foreground">
        {datapackAccessFooterLabel(dp)}
      </TableCell>

      <TableCell className="text-right">
        <div className="flex items-center justify-end gap-2">
          {/* Secondary first, primary last: Open is the action to land on. */}
          {onFileDatapack ? (
            <Button
              type="button"
              variant="secondary"
              size="sm"
              onClick={() => onFileDatapack(dp)}
            >
              {path ? MOVE_INTO_COLLECTION_LABEL : FILE_INTO_COLLECTION_LABEL}
            </Button>
          ) : null}
          {path || !onFileDatapack ? (
            <Button type="button" size="sm" onClick={() => onOpenDatapack(dp)}>
              Open
            </Button>
          ) : null}
        </div>
      </TableCell>
    </TableRow>
  );
}
