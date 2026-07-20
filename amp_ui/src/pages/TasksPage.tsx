// Tasks = the controller's async operation history (create project / branch /
// endpoint, etc.). A status filter + a paged table, most-recent-first. Read-only:
// the controller owns these rows; we only browse them here.

import { useState } from "react";
import { Badge } from "@/components/ui/badge";
import {
  HoverCard,
  HoverCardContent,
  HoverCardTrigger,
} from "@/components/ui/hover-card";
import {
  Pagination,
  PaginationContent,
  PaginationItem,
  PaginationNext,
  PaginationPrevious,
} from "@/components/ui/pagination";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  ErrorState,
  LoadingState,
  PageContainer,
  PageHeader,
} from "@/components/data/Page";
import { useTasks } from "@/lib/api/queries";
import type { Task, TaskStatus } from "@/lib/api/types";
import { formatDateTime, shortId } from "@/lib/format";

/** Page size for the task history. */
const PAGE_SIZE = 20;

/** Filter options — "all" is a sentinel (Radix Select forbids empty values);
 *  it maps to an undefined `status` param so the controller returns everything. */
const STATUS_FILTERS = ["all", "pending", "running", "succeeded", "failed"] as const;
type StatusFilter = (typeof STATUS_FILTERS)[number];

export default function TasksPage() {
  const [status, setStatus] = useState<StatusFilter>("all");
  const [offset, setOffset] = useState(0);

  const { data, isLoading, isError, error } = useTasks({
    limit: PAGE_SIZE,
    offset,
    status: status === "all" ? undefined : status,
  });

  const tasks = data?.tasks ?? [];
  const total = data?.total ?? 0;
  const count = data?.count ?? 0;
  const canPrev = offset > 0;
  const canNext = offset + count < total;

  function selectStatus(next: StatusFilter) {
    setStatus(next);
    setOffset(0);
  }

  return (
    <PageContainer>
      <PageHeader
        eyebrow="Task history"
        title="Tasks"
        description="Asynchronous operations, most recent first."
        action={
          <Select value={status} onValueChange={(v) => selectStatus(v as StatusFilter)}>
            <SelectTrigger className="w-40">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {STATUS_FILTERS.map((s) => (
                <SelectItem key={s} value={s} className="capitalize">
                  {s === "all" ? "All statuses" : s}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        }
      />

      {isError ? (
        <ErrorState error={error} />
      ) : isLoading ? (
        <LoadingState label="Loading tasks…" />
      ) : (
        <div className="rounded-xl border bg-card text-card-foreground shadow">
          <div className="flex items-center gap-2 border-b border-border px-5 py-4">
            <h2 className="font-semibold">Tasks</h2>
            <span className="text-sm text-muted-foreground">
              {total} total
            </span>
          </div>
          {tasks.length === 0 ? (
            <div className="px-5 py-12 text-center text-sm text-muted-foreground">
              {status === "all"
                ? "No tasks yet."
                : `No ${status} tasks.`}
            </div>
          ) : (
            <>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Status</TableHead>
                    <TableHead>Task</TableHead>
                    <TableHead>Cluster</TableHead>
                    <TableHead>Created</TableHead>
                    <TableHead>Updated</TableHead>
                    <TableHead>Error</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {tasks.map((task) => (
                    <TableRow key={task.id}>
                      <TableCell>
                        <StatusBadge status={task.status} />
                      </TableCell>
                      <TableCell title={task.kind}>
                        {taskKindLabel(task.kind)}
                      </TableCell>
                      <TableCell>{clusterLabel(task)}</TableCell>
                      <TableCell className="text-muted-foreground">
                        {formatDateTime(task.created_at)}
                      </TableCell>
                      <TableCell className="text-muted-foreground">
                        {formatDateTime(task.updated_at)}
                      </TableCell>
                      <TableCell>
                        {task.status === "failed" && task.error ? (
                          <ErrorCell error={task.error} />
                        ) : (
                          <span className="text-muted-foreground">—</span>
                        )}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>

              <div className="flex flex-wrap items-center justify-between gap-3 border-t border-border px-5 py-4">
                <span className="text-sm text-muted-foreground">
                  Showing {offset + 1}–{offset + count} of {total}
                </span>
                <Pagination className="mx-0 w-auto justify-end">
                  <PaginationContent>
                    <PaginationItem>
                      <PaginationPrevious
                        href="#"
                        aria-disabled={!canPrev}
                        className={
                          canPrev
                            ? "cursor-pointer"
                            : "pointer-events-none opacity-50"
                        }
                        onClick={(e) => {
                          e.preventDefault();
                          if (canPrev) setOffset((o) => Math.max(0, o - PAGE_SIZE));
                        }}
                      />
                    </PaginationItem>
                    <PaginationItem>
                      <PaginationNext
                        href="#"
                        aria-disabled={!canNext}
                        className={
                          canNext
                            ? "cursor-pointer"
                            : "pointer-events-none opacity-50"
                        }
                        onClick={(e) => {
                          e.preventDefault();
                          if (canNext) setOffset((o) => o + PAGE_SIZE);
                        }}
                      />
                    </PaginationItem>
                  </PaginationContent>
                </Pagination>
              </div>
            </>
          )}
        </div>
      )}
    </PageContainer>
  );
}

/** Human labels for controller task kinds. Creating or deleting a cluster spans
 *  a project task and an endpoint task, so both surface as "Create"/"Delete
 *  cluster". Unknown kinds fall back to a prettified form. */
const TASK_KIND_LABELS: Record<string, string> = {
  create_project: "Create cluster",
  branch: "Branch",
  create_endpoint: "Create cluster",
  delete_project: "Delete cluster",
  delete_branch: "Delete branch",
  delete_endpoint: "Delete cluster",
};

function taskKindLabel(kind: string): string {
  return TASK_KIND_LABELS[kind] ?? kind.replace(/_/g, " ").replace(/^\w/, (c) => c.toUpperCase());
}

/** Cluster column: the short cluster id, else the target ref, else a dash. */
function clusterLabel(task: Task): string {
  if (task.project_id) return shortId(task.project_id);
  if (task.target_ref) return shortId(task.target_ref);
  return "—";
}

/** Tailwind tint per status — matches the tinted-badge convention used
 *  elsewhere (e.g. MultitenancyPage). `failed` uses the destructive variant. */
const STATUS_CLASS: Record<Exclude<TaskStatus, "failed">, string> = {
  succeeded: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400",
  running: "bg-blue-500/15 text-blue-600 dark:text-blue-400",
  pending: "bg-muted text-muted-foreground",
};

function StatusBadge({ status }: { status: TaskStatus }) {
  if (status === "failed") {
    return (
      <Badge variant="destructive" className="capitalize">
        {status}
      </Badge>
    );
  }
  return <Badge className={`${STATUS_CLASS[status]} capitalize`}>{status}</Badge>;
}

/** Failed-task error: truncated inline, full text on hover. */
function ErrorCell({ error }: { error: string }) {
  return (
    <HoverCard>
      <HoverCardTrigger asChild>
        <span className="block max-w-xs cursor-default truncate text-destructive">
          {error}
        </span>
      </HoverCardTrigger>
      <HoverCardContent className="w-80 text-sm">
        <p className="break-words text-foreground">{error}</p>
      </HoverCardContent>
    </HoverCard>
  );
}
