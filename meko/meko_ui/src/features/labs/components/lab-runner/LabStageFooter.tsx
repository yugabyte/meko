// Sticky footer under the lab runner's centre pane: Back/Next, plus the
// reason Next is blocked when the current stage still has work outstanding.

import { Button } from "@/components/ui/button";

export function LabStageFooter({
  onBack,
  onNext,
  canGoBack,
  isLastStage,
  /** Why Next is disabled, or null when it isn't. */
  gateMessage,
}: {
  onBack: () => void;
  onNext: () => void;
  canGoBack: boolean;
  isLastStage: boolean;
  gateMessage: string | null;
}) {
  return (
    <div className="flex shrink-0 items-center gap-3 border-t border-border bg-background px-6 py-3">
      <Button variant="outline" size="sm" onClick={onBack} disabled={!canGoBack}>
        Back
      </Button>
      <span className="flex-1" />
      {gateMessage && (
        <p className="text-sm text-muted-foreground" role="status">
          {gateMessage}
        </p>
      )}
      {!isLastStage && (
        <Button size="sm" onClick={onNext} disabled={gateMessage !== null}>
          Next
        </Button>
      )}
    </div>
  );
}
