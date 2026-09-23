// The Datapacks table: every datapack with the collection it belongs to, its path
// inside that collection, and how many of its memories have been promoted.
//
// Replaces the old tile grid. A table because the point of this page after
// Collections is comparison across rows — which datapack sits where, and which
// ones are publishing nothing back.
//
// Paged in the browser, not on the server: the datapack list is already
// loaded in full for the sidebar and the filters, so slicing it costs a
// request nobody has to make. Same approach as the admin Users table.

import { useEffect, useState } from "react";
import {
  Table,
  TableBody,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Card } from "@/components/ui/card";
import {
  Pagination,
  PaginationContent,
  PaginationItem,
  PaginationNext,
  PaginationPrevious,
} from "@/components/ui/pagination";
import { useNowMs } from "@/lib/relativeTime";
import type { CurrentUser, Datapack } from "@/lib/api/mekoApi";
import type { DatapackPathMap } from "@/lib/api/collectionsApi";
import { DatapackTableRow } from "@/features/datapacks/components/datapacks-list/DatapackTableRow";

const COLUMNS = ["Datapack", "Collection", "Memories", "Promoted", "Last change", "Access"] as const;

/** Rows per page. The bar only appears above this, so a workspace with a
 *  handful of datapacks sees the plain list it always did. */
const PAGE_SIZE = 10;

export function DatapacksTable({
  datapacks,
  datapackPaths,
  currentUser,
  onOpenDatapack,
  onFileDatapack,
}: {
  datapacks: Datapack[];
  datapackPaths: DatapackPathMap;
  currentUser: CurrentUser | null;
  onOpenDatapack: (datapack: Datapack) => void;
  /** Omitted when there is nowhere to file into, which turns every row's
   *  action into Open rather than a dead button. */
  onFileDatapack?: (datapack: Datapack) => void;
}) {
  // One ticking clock for the whole table rather than one per row.
  const nowMs = useNowMs();
  const [page, setPage] = useState(1);

  // Filtering or searching shortens the list under the current page, which
  // would otherwise leave the user on an empty one.
  useEffect(() => {
    setPage(1);
  }, [datapacks.length]);

  const pageCount = Math.max(1, Math.ceil(datapacks.length / PAGE_SIZE));
  const currentPage = Math.min(page, pageCount);
  const start = (currentPage - 1) * PAGE_SIZE;
  const rows = datapacks.slice(start, start + PAGE_SIZE);
  const showPagination = datapacks.length > PAGE_SIZE;

  return (
    <div className="space-y-3">
      <Card className="overflow-hidden border-border bg-card p-0">
        <Table>
          <TableHeader>
            <TableRow>
              {COLUMNS.map((column) => (
                <TableHead key={column} className="text-xs uppercase tracking-wide">
                  {column}
                </TableHead>
              ))}
              <TableHead className="sr-only">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((dp) => (
              <DatapackTableRow
                key={dp.datapack_id}
                dp={dp}
                path={datapackPaths[dp.datapack_id]}
                currentUser={currentUser}
                nowMs={nowMs}
                onOpenDatapack={onOpenDatapack}
                onFileDatapack={onFileDatapack}
              />
            ))}
          </TableBody>
        </Table>
      </Card>

      {showPagination ? (
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className="text-sm text-muted-foreground">
            Showing {start + 1}&ndash;{Math.min(start + PAGE_SIZE, datapacks.length)} of{" "}
            {datapacks.length}
          </p>
          <Pagination className="mx-0 w-auto justify-end">
            <PaginationContent>
              <PaginationItem>
                <PaginationPrevious
                  aria-disabled={currentPage <= 1}
                  className={currentPage <= 1 ? "pointer-events-none opacity-50" : "cursor-pointer"}
                  onClick={() => setPage(Math.max(1, currentPage - 1))}
                />
              </PaginationItem>
              <PaginationItem>
                <PaginationNext
                  aria-disabled={currentPage >= pageCount}
                  className={
                    currentPage >= pageCount ? "pointer-events-none opacity-50" : "cursor-pointer"
                  }
                  onClick={() => setPage(Math.min(pageCount, currentPage + 1))}
                />
              </PaginationItem>
            </PaginationContent>
          </Pagination>
      </div>
      ) : null}
    </div>
  );
}
