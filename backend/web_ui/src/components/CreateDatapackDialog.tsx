// Creates a datapack by name (3 to 60 characters) and an optional description.

import { useState } from "react";
import { api } from "../lib/api";
import type { Datapack } from "../lib/types";
import { Dialog } from "./Dialog";
import { Button, ErrorNote, Input } from "./ui";

export function CreateDatapackDialog({
  onClose,
  onCreated,
}: {
  onClose: () => void;
  onCreated: (d: Datapack) => void;
}) {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error>();

  const submit = async () => {
    setBusy(true);
    setError(undefined);
    try {
      onCreated(await api.createDatapack(name.trim(), description.trim()));
    } catch (e) {
      setError(e as Error);
      setBusy(false);
    }
  };

  return (
    <Dialog
      title="New datapack"
      onClose={onClose}
      actions={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={busy || name.trim().length < 3} onClick={submit}>
            Create
          </Button>
        </>
      }
    >
      <label className="block space-y-1 text-sm">
        <span className="font-medium">Name</span>
        <Input autoFocus value={name} maxLength={60} onChange={(e) => setName(e.target.value)} />
      </label>
      <label className="block space-y-1 text-sm">
        <span className="font-medium">Description</span>
        <Input value={description} onChange={(e) => setDescription(e.target.value)} placeholder="Optional" />
      </label>
      {error && <ErrorNote error={error} />}
    </Dialog>
  );
}
