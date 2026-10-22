// The knowledge base's timeline, newest first: files uploaded (with how many chunks they indexed)
// and memories promoted into it. Loads 20 at a time.

import { useEffect, useState } from "react";
import { FileText, Sparkles } from "lucide-react";
import { api } from "../lib/api";
import { bytes, timeAgo } from "../lib/format";
import type { KnowledgeActivity } from "../lib/types";
import { Button, Card, ErrorNote, Spinner } from "./ui";

// A file's activity names it by its key, which ends in the file's own name.
const fileName = (key = "") => key.slice(key.lastIndexOf("/") + 1) || key;

export function KnowledgeActivityLog({ datapackId, refresh }: { datapackId: string; refresh: string }) {
  const [items, setItems] = useState<KnowledgeActivity[]>([]);
  const [cursor, setCursor] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error>();

  const load = async (from: string) => {
    setBusy(true);
    try {
      const page = await api.activity(datapackId, from);
      setItems((prev) => (from ? [...prev, ...page.items] : page.items));
      setCursor(page.has_more ? page.next_cursor : "");
      setError(undefined);
    } catch (e) {
      setError(e as Error);
    } finally {
      setBusy(false);
    }
  };

  // Reload from the newest whenever the files change.
  useEffect(() => {
    void load("");
  }, [datapackId, refresh]);

  return (
    <section className="space-y-2">
      <h3 className="text-sm font-medium">Activity</h3>
      {error && <ErrorNote error={error} />}
      {items.length === 0 && !busy && !error ? (
        <p className="text-sm text-muted-foreground">Nothing yet: uploads and promoted memories show here.</p>
      ) : (
        <Card className="divide-y text-sm">
          {items.map((a, i) => (
            <div key={`${a.timestamp}-${i}`} className="flex items-start gap-3 px-3 py-2">
              {a.type === "memory_promotion" ? (
                <Sparkles className="mt-0.5 size-4 shrink-0 text-info" />
              ) : (
                <FileText className="mt-0.5 size-4 shrink-0 text-success" />
              )}
              <div className="min-w-0 flex-1">
                {a.type === "memory_promotion" ? (
                  <p>
                    Promoted a memory: <span className="text-muted-foreground">“{a.content}”</span>
                  </p>
                ) : (
                  <p>
                    Uploaded <b>{fileName(a.document_name)}</b>
                    <span className="text-muted-foreground">
                      {a.chunks_processed !== undefined && ` · ${a.chunks_processed} chunks`}
                      {a.size_bytes !== undefined && ` · ${bytes(a.size_bytes)}`}
                    </span>
                  </p>
                )}
              </div>
              <span className="shrink-0 text-xs text-muted-foreground">{timeAgo(a.timestamp)}</span>
            </div>
          ))}
        </Card>
      )}
      {busy && <Spinner />}
      {cursor && !busy && (
        <Button size="sm" variant="ghost" onClick={() => void load(cursor)}>
          Show older
        </Button>
      )}
    </section>
  );
}
