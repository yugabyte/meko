// Asks before an action that can't be undone; given a name, it's done only once that name is typed,
// as deleting a datapack or a project asks.

import { useState, type ReactNode } from "react";
import { Dialog } from "./Dialog";
import { Button, ErrorNote, Input } from "./ui";

export function ConfirmDialog({
  title,
  name,
  action,
  onClose,
  onConfirm,
  children,
}: {
  title: string;
  name?: string;
  action: string;
  onClose: () => void;
  onConfirm: () => Promise<void>;
  children: ReactNode;
}) {
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error>();
  const confirm = async () => {
    setBusy(true);
    try {
      await onConfirm();
    } catch (e) {
      setError(e as Error);
      setBusy(false);
    }
  };
  return (
    <Dialog
      title={title}
      onClose={onClose}
      actions={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            className="bg-destructive text-white"
            disabled={busy || (name !== undefined && typed !== name)}
            onClick={confirm}
          >
            {action}
          </Button>
        </>
      }
    >
      <div className="text-sm">{children}</div>
      {name !== undefined && (
        <Input autoFocus value={typed} onChange={(e) => setTyped(e.target.value)} aria-label="Type the name to confirm" />
      )}
      {error && <ErrorNote error={error} />}
    </Dialog>
  );
}
