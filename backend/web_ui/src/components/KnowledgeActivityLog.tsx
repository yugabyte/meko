// The knowledge base's timeline, newest first: files uploaded (with how many chunks they indexed)
// and memories promoted into it. Loads 20 at a time.

import { useEffect, useRef, useState } from "react";
import { FileText, Sparkles } from "lucide-react";
import { api } from "../lib/api";
import { SHOW_COLLECTIVE } from "../lib/features";
import { bytes, timeAgo } from "../lib/format";
import type { KnowledgeActivity } from "../lib/types";
import { Button, Card, ErrorNote, Spinner } from "./ui";

// A file's activity names it by its key, which ends in the file's own name.
const fileName = (key = "") => key.slice(key.lastIndexOf("/") + 1) || key;

export function KnowledgeActivityLog({ datapackId, refresh }: { datapackId: string; refresh: number }) {
  const [items, setItems] = useState<KnowledgeActivity[]>([]);
  const [cursor, setCursor] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error>();
  // Each load's number: a reply to an older one (another datapack, or before a reload) is dropped.
  const latest = useRef(0);

  const load = async (from: string) => {
    const mine = ++latest.current;
    setBusy(true);
    try {
      const page = await api.activity(datapackId, from);
      if (mine !== latest.current) return;
      const shown = SHOW_COLLECTIVE ? page.items : page.items.filter((a) => a.type !== "memory_promotion");
      setItems((prev) => (from ? [...prev, ...shown] : shown));
      setCursor(page.has_more ? page.next_cursor : "");
      setError(undefined);
    } catch (e) {
      if (mine === latest.current) setError(e as Error);
    } finally {
      if (mine === latest.current) setBusy(false);
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
        <p className="text-sm text-muted-foreground">Nothing yet: {SHOW_COLLECTIVE ? "uploads and promoted memories show here" : "uploads show here"}.</p>
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
