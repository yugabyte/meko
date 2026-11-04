// A modal dialog: a title, its body, and actions. Escape or the backdrop closes it.

import { useEffect, type ReactNode } from "react";
import { X } from "lucide-react";
import { Button } from "./ui";

export function Dialog({
  title,
  onClose,
  children,
  actions,
}: {
  title: string;
  onClose: () => void;
  children: ReactNode;
  actions: ReactNode;
}) {
  useEffect(() => {
    const on = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", on);
    return () => window.removeEventListener("keydown", on);
  }, [onClose]);
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4" onMouseDown={onClose}>
      <div
        role="dialog"
        aria-label={title}
        className="flex max-h-[90dvh] w-full max-w-md flex-col rounded-lg border bg-card shadow-xl"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between border-b px-4 py-3">
          <h2 className="font-semibold">{title}</h2>
          <Button size="sm" variant="ghost" onClick={onClose} aria-label="Close">
            <X className="size-4" />
          </Button>
        </div>
        <div className="min-h-0 space-y-3 overflow-y-auto p-4">{children}</div>
        <div className="flex justify-end gap-2 border-t px-4 py-3">{actions}</div>
      </div>
    </div>
  );
}
