// Adds a folder to a project, or edits a folder's name and description when given it.

import { useState } from "react";
import { api } from "../lib/api";
import type { ProjectFolder } from "../lib/types";
import { Dialog } from "./Dialog";
import { NAME_MAX, nameOK } from "./ProjectDialog";
import { Button, ErrorNote, Field, Input } from "./ui";

export function FolderDialog({
  projectId,
  folder,
  onClose,
  onSaved,
}: {
  projectId: string;
  folder?: ProjectFolder;
  onClose: () => void;
  onSaved: (id: string) => void;
}) {
  const [name, setName] = useState(folder?.folder_name ?? "");
  const [description, setDescription] = useState(folder?.folder_description ?? "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error>();

  const submit = async () => {
    setBusy(true);
    setError(undefined);
    const body = { folder_name: name.trim(), folder_description: description.trim() };
    try {
      if (folder) {
        await api.updateFolder(projectId, folder.folder_id, body);
        onSaved(folder.folder_id);
      } else {
        onSaved((await api.createFolder(projectId, body)).folder_id);
      }
    } catch (e) {
      setError(e as Error);
      setBusy(false);
    }
  };

  return (
    <Dialog
      title={folder ? "Edit folder" : "New folder"}
      onClose={onClose}
      actions={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={busy || !nameOK(name)} onClick={submit}>
            {folder ? "Save" : "Add"}
          </Button>
        </>
      }
    >
      <Field label="Name">
        <Input autoFocus value={name} maxLength={NAME_MAX} onChange={(e) => setName(e.target.value)} />
      </Field>
      <Field label="Description" hint="Helps you and your agents pick the right folder later.">
        <Input value={description} onChange={(e) => setDescription(e.target.value)} placeholder="Optional" />
      </Field>
      {error && <ErrorNote error={error} />}
    </Dialog>
  );
}
