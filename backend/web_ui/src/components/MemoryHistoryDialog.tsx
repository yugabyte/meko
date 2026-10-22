// A memory's changes, oldest first: when it was added, each update with its old and new text, and
// its delete, as mem0 recorded them.

import { api } from "../lib/api";
import { dateTime } from "../lib/format";
import type { MemoryChange } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { Dialog } from "./Dialog";
import { Badge, Button, ErrorNote, Loading } from "./ui";

const TONE: Record<string, "success" | "primary" | "danger"> = { ADD: "success", UPDATE: "primary", DELETE: "danger" };

export function MemoryHistoryDialog({
  datapackId,
  memoryId,
  onClose,
}: {
  datapackId: string;
  memoryId: string;
  onClose: () => void;
}) {
  const { data, error, loading } = useLoad(() => api.memoryHistory(datapackId, memoryId), [datapackId, memoryId]);
  return (
    <Dialog title="Memory history" onClose={onClose} actions={<Button onClick={onClose}>Close</Button>}>
      {loading && !data ? (
        <Loading />
      ) : error ? (
        <ErrorNote error={error} />
      ) : (
        <ol className="space-y-3">
          {(data?.history ?? []).map((h: MemoryChange, i) => (
            <li key={i} className="border-l-2 pl-3">
              <div className="flex items-center gap-2 text-xs text-muted-foreground">
                <Badge tone={TONE[h.action] ?? "primary"}>{h.action.toLowerCase()}</Badge>
                {dateTime(h.updated_at ?? h.created_at)}
              </div>
              {h.action === "UPDATE" && h.previous_value !== null && (
                <p className="mt-1 text-sm text-muted-foreground line-through">{h.previous_value}</p>
              )}
              {h.new_value !== null && h.action !== "DELETE" && <p className="mt-1 text-sm">{h.new_value}</p>}
            </li>
          ))}
        </ol>
      )}
    </Dialog>
  );
}
