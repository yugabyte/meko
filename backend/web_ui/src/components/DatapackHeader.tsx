// A datapack's name, description and tabs, with delete.

import { useState } from "react";
import { Download, Pencil, Trash2, UserPlus } from "lucide-react";
import { api } from "../lib/api";
import { saveBlob } from "../lib/download";
import { href, navigate, type Tab } from "../lib/route";
import type { Datapack } from "../lib/types";
import { CloudBanner } from "./CloudBanner";
import { DatapackDialog } from "./DatapackDialog";
import { Dialog } from "./Dialog";
import { Button, cx, ErrorNote, Input } from "./ui";

const LABELS: Record<Tab, string> = {
  overview: "Overview",
  metrics: "Metrics",
  conversations: "Conversations",
  memories: "Memories",
  knowledge: "Knowledge",
  artifacts: "Artifacts",
  search: "Search",
  connect: "Connect",
};

export function DatapackHeader({
  datapack,
  tab,
  onEdited,
  onDeleted,
}: {
  datapack: Datapack;
  tab: Tab;
  onEdited: () => void;
  onDeleted: () => void;
}) {
  const [deleting, setDeleting] = useState(false);
  const [editing, setEditing] = useState(false);
  const [exporting, setExporting] = useState(false);
  const exportIt = async () => {
    setExporting(true);
    try {
      const { filename, blob } = await api.exportDatapack(datapack.datapack_id);
      saveBlob(filename, blob);
    } catch (e) {
      alert(`Export failed: ${(e as Error).message}`);
    } finally {
      setExporting(false);
    }
  };
  const [inviting, setInviting] = useState(false);
  return (
    <header className="border-b bg-card px-8 pt-6">
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <h1 className="truncate text-xl font-semibold">{datapack.datapack_name}</h1>
          {datapack.description && <p className="mt-0.5 text-sm text-muted-foreground">{datapack.description}</p>}
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <Button
            size="sm"
            variant="ghost"
            disabled={exporting}
            onClick={exportIt}
            title="Download this datapack as a zip: memories, conversations, knowledge files and artifacts, with their embeddings and file contents, and the call log"
          >
            <Download className="size-3.5" /> Export
          </Button>
          <Button size="sm" variant="ghost" onClick={() => setEditing(true)} title="Edit name and description">
            <Pencil className="size-3.5" /> Edit
          </Button>
          <Button size="sm" onClick={() => setInviting(true)} title="Invite teammates">
            <UserPlus className="size-3.5" /> Invite
          </Button>
          <Button variant="danger" size="sm" onClick={() => setDeleting(true)}>
            <Trash2 className="size-3.5" /> Delete
          </Button>
        </div>
      </div>
      {inviting && <CloudBanner onClose={() => setInviting(false)} />}
      <nav className="mt-5 flex gap-5 text-sm">
        {(Object.keys(LABELS) as Tab[]).map((t) => (
          <a
            key={t}
            href={href({ datapackId: datapack.datapack_id, tab: t })}
            onClick={(e) => {
              e.preventDefault();
              navigate({ datapackId: datapack.datapack_id, tab: t });
            }}
            className={cx(
              "-mb-px border-b-2 pb-2.5",
              t === tab ? "border-primary font-medium" : "border-transparent text-muted-foreground hover:text-foreground",
            )}
          >
            {LABELS[t]}
          </a>
        ))}
      </nav>
      {editing && (
        <DatapackDialog
          datapack={datapack}
          onClose={() => setEditing(false)}
          onSaved={() => {
            setEditing(false);
            onEdited();
          }}
        />
      )}
      {deleting && <DeleteDialog datapack={datapack} onClose={() => setDeleting(false)} onDeleted={onDeleted} />}
    </header>
  );
}

// Deleting takes everything in the datapack, so it asks for the name.
function DeleteDialog({ datapack, onClose, onDeleted }: { datapack: Datapack; onClose: () => void; onDeleted: () => void }) {
  const [typed, setTyped] = useState("");
  const [error, setError] = useState<Error>();
  const remove = async () => {
    try {
      await api.deleteDatapack(datapack.datapack_id);
      onDeleted();
    } catch (e) {
      setError(e as Error);
    }
  };
  return (
    <Dialog
      title="Delete datapack"
      onClose={onClose}
      actions={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            className="bg-destructive text-white"
            disabled={typed !== datapack.datapack_name}
            onClick={remove}
          >
            Delete for good
          </Button>
        </>
      }
    >
      <p className="text-sm">
        This removes <b>{datapack.datapack_name}</b> and every memory, conversation and knowledge file in it. It can't
        be undone. Type its name to confirm.
      </p>
      <Input autoFocus value={typed} onChange={(e) => setTyped(e.target.value)} />
      {error && <ErrorNote error={error} />}
    </Dialog>
  );
}
