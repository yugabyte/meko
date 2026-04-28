/**
 * Sticky bottom actions for the configure wizard: Back (or exit to datapacks), optional Skip step, and Next.
 * The Finish step uses the footer card above for the primary action.
 */
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useKbImportJobDetail } from "@/hooks/useKbImportJob";
import { WIZARD_STEP_ID, type WizardStepId } from "./wizardSteps";

type Props = {
  stepId: WizardStepId;
  currentIndex: number;
  /** Overrides default “Back” / “Back to datapacks” label from the parent when needed. */
  backButtonLabel?: string;
  onBack: () => void;
  onNext: () => void;
  /** Underlined control to the left of Next (install/sync: next step; finish: exit without KB when uploads exist). */
  onSkipStep?: () => void;
  /** When false on Add Knowledge, Next is disabled (e.g. uploads not finished). Use Skip step to bypass. */
  canGoNext?: boolean;
  nextDisabledTooltip?: string;
};

export function ConfigureWizardNavBar({
  stepId,
  currentIndex,
  backButtonLabel,
  onBack,
  onNext,
  onSkipStep,
  canGoNext = true,
  nextDisabledTooltip = "Upload all knowledge files before continuing, or use Skip step.",
}: Props) {
  const kbJob = useKbImportJobDetail();
  const importRunning =
    stepId === WIZARD_STEP_ID.import && (kbJob.phase === "creating" || kbJob.phase === "polling");
  const showSkipLink = onSkipStep != null && !importRunning;

  const onFinishStep = stepId === WIZARD_STEP_ID.import;
  const nextDisabled = !onFinishStep && !canGoNext;
  const nextButton = (
    <Button type="button" disabled={nextDisabled} onClick={onNext}>
      Next
    </Button>
  );

  const nextCluster =
    onFinishStep ? null : nextDisabled ? (
      <Tooltip>
        <TooltipTrigger asChild>
          <span className="inline-flex">{nextButton}</span>
        </TooltipTrigger>
        <TooltipContent side="top">{nextDisabledTooltip}</TooltipContent>
      </Tooltip>
    ) : (
      nextButton
    );

  return (
    <div className="sticky bottom-0 z-[1] mt-4 flex w-full flex-wrap items-center justify-between gap-3 border-t border-border bg-background/95 py-4 backdrop-blur supports-[backdrop-filter]:bg-background/80">
      <Button type="button" variant="outline" onClick={onBack}>
        {backButtonLabel ?? (currentIndex === 0 ? "Back to datapacks" : "Back")}
      </Button>
      <div className="flex flex-wrap items-center justify-end gap-4">
        {showSkipLink ? (
          <button
            type="button"
            className="text-sm text-muted-foreground underline underline-offset-4 transition hover:text-foreground"
            onClick={onSkipStep}
          >
            Skip step
          </button>
        ) : null}
        {!onFinishStep ? nextCluster : null}
      </div>
    </div>
  );
}
