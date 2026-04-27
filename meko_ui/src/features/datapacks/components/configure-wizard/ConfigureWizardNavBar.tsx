/**
 * Sticky bottom actions for the configure wizard: Back (or exit to datapacks) and Next / Open datapack.
 * Stays visible while scrolling long install or sync content.
 */
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { WIZARD_STEP_ID, type WizardStepId } from "./wizardSteps";

type Props = {
  stepId: WizardStepId;
  currentIndex: number;
  onBack: () => void;
  onNext: () => void;
  onOpenDatapack: () => void;
  /** When false on Sync Knowledge, Next is disabled (e.g. uploads not finished). */
  canGoNext?: boolean;
  nextDisabledTooltip?: string;
};

export function ConfigureWizardNavBar({
  stepId,
  currentIndex,
  onBack,
  onNext,
  onOpenDatapack,
  canGoNext = true,
  nextDisabledTooltip = "Upload all knowledge files before continuing.",
}: Props) {
  const nextDisabled = stepId !== WIZARD_STEP_ID.import && !canGoNext;
  const nextButton = (
    <Button type="button" disabled={nextDisabled} onClick={onNext}>
      Next
    </Button>
  );

  return (
    <div className="sticky bottom-0 z-[1] mt-4 flex w-full flex-wrap items-center justify-between gap-3 border-t border-border bg-background/95 py-4 backdrop-blur supports-[backdrop-filter]:bg-background/80">
      <Button type="button" variant="outline" onClick={onBack}>
        {currentIndex === 0 ? "Back to datapacks" : "Back"}
      </Button>
      {stepId === WIZARD_STEP_ID.import ? (
        <Button type="button" className="bg-white text-black hover:bg-gray-200" onClick={onOpenDatapack}>
          Open datapack
        </Button>
      ) : nextDisabled ? (
        <Tooltip>
          <TooltipTrigger asChild>
            <span className="inline-flex">{nextButton}</span>
          </TooltipTrigger>
          <TooltipContent side="top">{nextDisabledTooltip}</TooltipContent>
        </Tooltip>
      ) : (
        nextButton
      )}
    </div>
  );
}
