/**
 * Sticky bottom actions for the configure wizard: Back (or exit to datapacks) and Next / Open datapack.
 * Stays visible while scrolling long install or sync content.
 */
import { Button } from "@/components/ui/button";
import { WIZARD_STEP_ID, type WizardStepId } from "./wizardSteps";

type Props = {
  stepId: WizardStepId;
  currentIndex: number;
  onBack: () => void;
  onNext: () => void;
  onOpenDatapack: () => void;
};

export function ConfigureWizardNavBar({ stepId, currentIndex, onBack, onNext, onOpenDatapack }: Props) {
  return (
    <div className="sticky bottom-0 z-[1] mt-4 flex w-full flex-wrap items-center justify-between gap-3 border-t border-border bg-background/95 py-4 backdrop-blur supports-[backdrop-filter]:bg-background/80">
      <Button type="button" variant="outline" onClick={onBack}>
        {currentIndex === 0 ? "Back to datapacks" : "Back"}
      </Button>
      {stepId === WIZARD_STEP_ID.status ? (
        <Button type="button" className="bg-white text-black hover:bg-gray-200" onClick={onOpenDatapack}>
          Open datapack
        </Button>
      ) : (
        <Button type="button" onClick={onNext}>
          Next
        </Button>
      )}
    </div>
  );
}
