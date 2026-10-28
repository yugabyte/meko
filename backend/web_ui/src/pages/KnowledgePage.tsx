// The knowledge base: drop or pick files to upload, watch them index, download or delete them, and
// its activity.

import { useEffect, useRef, useState, type DragEvent } from "react";
import { Download, FileText, Trash2, UploadCloud } from "lucide-react";
import { api } from "../lib/api";
import { bytes, timeAgo } from "../lib/format";
import type { Datapack, KnowledgeDocument } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { KnowledgeActivityLog } from "../components/KnowledgeActivityLog";
import { Table } from "../components/Table";
import { Badge, Button, Card, cx, Empty, ErrorNote, Input, Loading, Spinner } from "../components/ui";

// What Meko indexes (backend/internal/knowledge), and its size limit.
const ACCEPT = ".pdf,.txt,.text,.md,.csv,.json,.xml,.html,.htm";
const MAX_BYTES = 50 * 1024 * 1024;

const STATUS: Record<string, { label: string; tone: "neutral" | "warning" | "success" | "danger" }> = {
  QUEUED: { label: "Queued", tone: "neutral" },
  PROCESSING: { label: "Indexing", tone: "warning" },
  COMPLETED: { label: "Indexed", tone: "success" },
  FAILED: { label: "Failed", tone: "danger" },
};

interface Upload {
  name: string;
  state: "uploading" | "done" | "error";
  message?: string;
}

export function KnowledgePage({ datapack, onChanged }: { datapack: Datapack; onChanged: () => void }) {
  const id = datapack.datapack_id;
  const [q, setQ] = useState("");
  const [uploads, setUploads] = useState<Upload[]>([]);
  const [dragging, setDragging] = useState(false);
  const [actionError, setActionError] = useState<Error>();
  const picker = useRef<HTMLInputElement>(null);
  const docs = useLoad(() => api.documents(id, q), [id, q]);

  // Poll while anything is still indexing.
  const busy = docs.data?.items.some((d) => d.status === "QUEUED" || d.status === "PROCESSING");
  useEffect(() => {
    if (!busy) return;
    const t = setInterval(docs.reload, 2000);
    return () => clearInterval(t);
  }, [busy, docs.reload]);
  // The datapack's counts change when indexing finishes.
  const wasBusy = useRef(false);
  // Bumped when a file is uploaded, deleted or done indexing: what the activity log reloads on
  // (not the name filter, which doesn't filter the log).
  const [changes, setChanges] = useState(0);
  useEffect(() => {
    if (wasBusy.current && !busy) {
      onChanged();
      setChanges((n) => n + 1);
    }
    wasBusy.current = !!busy;
  }, [busy, onChanged]);

  const setUpload = (name: string, u: Partial<Upload>) =>
    setUploads((all) => all.map((x) => (x.name === name ? { ...x, ...u } : x)));

  const uploadFiles = async (files: File[]) => {
    setUploads((all) => [...files.map((f) => ({ name: f.name, state: "uploading" as const })), ...all]);
    for (const f of files) {
      if (f.size > MAX_BYTES) {
        setUpload(f.name, { state: "error", message: "larger than 50 MB" });
        continue;
      }
      try {
        const offer = await api.upload(id, f);
        setUpload(f.name, {
          state: "done",
          message: offer.already_uploaded ? `already here as ${offer.existing_filename}` : undefined,
        });
      } catch (e) {
        setUpload(f.name, { state: "error", message: (e as Error).message });
      }
      docs.reload();
      setChanges((n) => n + 1);
    }
  };

  const onDrop = (e: DragEvent) => {
    e.preventDefault();
    setDragging(false);
    void uploadFiles(Array.from(e.dataTransfer.files));
  };

  const download = async (d: KnowledgeDocument) => {
    try {
      const { url } = await api.downloadURL(id, d.document_uri);
      window.open(new URL(url).pathname, "_blank", "noopener");
    } catch (e) {
      setActionError(e as Error);
    }
  };

  const remove = async (d: KnowledgeDocument) => {
    if (!confirm(`Delete ${d.filename} and its ${d.chunks_processed} chunks?`)) return;
    try {
      await api.deleteDocument(id, d.id);
      docs.reload();
      setChanges((n) => n + 1);
      onChanged();
    } catch (e) {
      setActionError(e as Error);
    }
  };

  return (
    <div className="space-y-4">
      <div
        onDragOver={(e) => (e.preventDefault(), setDragging(true))}
        onDragLeave={() => setDragging(false)}
        onDrop={onDrop}
        onClick={() => picker.current?.click()}
        className={cx(
          "flex cursor-pointer flex-col items-center gap-2 rounded-lg border-2 border-dashed p-8 text-center transition",
          dragging ? "border-primary bg-primary/5" : "hover:border-primary/60",
        )}
      >
        <UploadCloud className="size-7 text-primary" />
        <p className="text-sm font-medium">Drop files here, or click to choose</p>
        <p className="text-xs text-muted-foreground">PDF, text, Markdown, CSV, JSON, XML or HTML, up to 50 MB each</p>
        <input
          ref={picker}
          type="file"
          multiple
          accept={ACCEPT}
          className="hidden"
          onChange={(e) => {
            void uploadFiles(Array.from(e.target.files ?? []));
            e.target.value = "";
          }}
        />
      </div>

      {uploads.length > 0 && (
        <Card className="divide-y text-sm">
          {uploads.slice(0, 5).map((u, i) => (
            <div key={u.name + i} className="flex items-center gap-2 px-3 py-2">
              {u.state === "uploading" ? <Spinner /> : <FileText className="size-4 text-muted-foreground" />}
              <span className="truncate">{u.name}</span>
              <span className={cx("ml-auto shrink-0 text-xs", u.state === "error" ? "text-destructive" : "text-muted-foreground")}>
                {u.state === "uploading" ? "uploading…" : u.state === "error" ? u.message : (u.message ?? "uploaded")}
              </span>
            </div>
          ))}
        </Card>
      )}

      {actionError && <ErrorNote error={actionError} />}

      <div className="flex items-center justify-between gap-3">
        <h3 className="text-sm font-medium">
          Files {docs.data && <span className="text-muted-foreground">({docs.data.total})</span>}
        </h3>
        <Input className="max-w-xs" placeholder="Filter by name" value={q} onChange={(e) => setQ(e.target.value)} />
      </div>

      {docs.loading && !docs.data ? (
        <Loading />
      ) : docs.error ? (
        <ErrorNote error={docs.error} />
      ) : !docs.data?.items.length ? (
        <Empty icon={<FileText className="size-8" />} title={q ? "No files match" : "No knowledge files yet"}>
          Upload here, with <b>mekosh upload</b>, or from an agent with <b>knowledgebase_upload_file</b>.
        </Empty>
      ) : (
        <Card className="overflow-hidden">
          <Table
            rows={docs.data.items}
            rowKey={(d) => d.id}
            columns={[
              { label: "Name", cell: (d) => <span title={d.filename}>{d.filename}</span>, className: "max-w-xs truncate" },
              {
                label: "Status",
                cell: (d) => {
                  const s = STATUS[d.status] ?? { label: d.status, tone: "neutral" as const };
                  return (
                    <span title={d.error}>
                      <Badge tone={s.tone}>{s.label}</Badge>
                    </span>
                  );
                },
              },
              { label: "Chunks", right: true, cell: (d) => d.chunks_processed },
              { label: "Size", right: true, cell: (d) => bytes(d.size_bytes) },
              { label: "Uploaded", cell: (d) => timeAgo(d.uploaded_at), className: "text-muted-foreground" },
              {
                label: "",
                cell: (d) => (
                  <div className="flex justify-end gap-1 opacity-60 group-hover:opacity-100">
                    <Button size="sm" variant="ghost" title="Download" onClick={() => download(d)}>
                      <Download className="size-3.5" />
                    </Button>
                    <Button size="sm" variant="danger" title="Delete" onClick={() => remove(d)}>
                      <Trash2 className="size-3.5" />
                    </Button>
                  </div>
                ),
              },
            ]}
          />
        </Card>
      )}

      <KnowledgeActivityLog
        datapackId={id}
        refresh={changes}
      />
    </div>
  );
}
