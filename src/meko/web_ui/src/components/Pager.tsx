// Previous and next for a paged list, with an optional "Page 2 of 5".

import { Button } from "./ui";

export function Pager({
  label,
  canPrev,
  canNext,
  onPrev,
  onNext,
}: {
  label?: string;
  canPrev: boolean;
  canNext: boolean;
  onPrev: () => void;
  onNext: () => void;
}) {
  if (!canPrev && !canNext) return null;
  return (
    <div className="flex items-center justify-end gap-2 text-sm text-muted-foreground">
      {label}
      <Button size="sm" disabled={!canPrev} onClick={onPrev}>
        Previous
      </Button>
      <Button size="sm" disabled={!canNext} onClick={onNext}>
        Next
      </Button>
    </div>
  );
}
