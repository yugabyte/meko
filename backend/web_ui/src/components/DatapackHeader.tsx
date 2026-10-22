// A datapack's name, description and tabs, with delete.

import { useState } from "react";
import { Trash2 } from "lucide-react";
import { api } from "../lib/api";
import { href, navigate, type Tab } from "../lib/route";
import type { Datapack } from "../lib/types";
import { Dialog } from "./Dialog";
import { Button, cx, ErrorNote, Input } from "./ui";

const LABELS: Record<Tab, string> = {
  overview: "Overview",
  search: "Search",
  memories: "Memories",
  knowledge: "Knowledge",
  conversations: "Conversations",
  connect: "Connect",
};

export function DatapackHeader({
  datapack,
  tab,
  onDeleted,
}: {
  datapack: Datapack;
  tab: Tab;
  onDeleted: () => void;
}) {
  const [deleting, setDeleting] = useState(false);
  return (
    <header className="border-b bg-card px-8 pt-6">
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <h1 className="truncate text-xl font-semibold">{datapack.datapack_name}</h1>
          {datapack.description && <p className="mt-0.5 text-sm text-muted-foreground">{datapack.description}</p>}
        </div>
        <Button variant="danger" size="sm" onClick={() => setDeleting(true)}>
          <Trash2 className="size-3.5" /> Delete
        </Button>
      </div>
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
