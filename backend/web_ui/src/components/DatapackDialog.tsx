// Creates a datapack, or edits one's name and description when given it. Names are 3 to 60
// characters. While Meko Cloud is linked, a new datapack is made there: Meko makes none on this
// computer then.

import { useState } from "react";
import { api } from "../lib/api";
import type { Datapack } from "../lib/types";
import { Dialog } from "./Dialog";
import { Button, ErrorNote, Input } from "./ui";

export function DatapackDialog({
  datapack,
  inCloud = false,
  onClose,
  onSaved,
}: {
  datapack?: Datapack;
  // Create it in the linked Meko Cloud.
  inCloud?: boolean;
  onClose: () => void;
  // The new datapack's id when one was created, else the edited one's.
  onSaved: (id: string) => void;
}) {
  const [name, setName] = useState(datapack?.datapack_name ?? "");
  const [description, setDescription] = useState(datapack?.description ?? "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error>();

  const submit = async () => {
    setBusy(true);
    setError(undefined);
    try {
      if (datapack) {
        await api.updateDatapack(datapack.datapack_id, { name: name.trim(), description: description.trim() });
        onSaved(datapack.datapack_id);
      } else {
        onSaved((await api.createDatapack(name.trim(), description.trim(), inCloud)).datapack_id);
      }
    } catch (e) {
      setError(e as Error);
      setBusy(false);
    }
  };

  return (
    <Dialog
      title={datapack ? "Edit datapack" : inCloud ? "New Meko Cloud datapack" : "New datapack"}
      onClose={onClose}
      actions={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={busy || name.trim().length < 3} onClick={submit}>
            {datapack ? "Save" : "Create"}
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
      {!datapack && inCloud && (
        <p className="text-sm text-muted-foreground">
          Meko is linked to Meko Cloud, so the datapack is made there. It opens in Meko Cloud.
        </p>
      )}
      {error && <ErrorNote error={error} />}
    </Dialog>
  );
}
