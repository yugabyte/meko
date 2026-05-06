/**
 * Single sticky footer for the configure wizard: datapack name (left), optional counts (center), actions (right).
 */
import type { ReactNode } from "react";
import { ArrowRight, Loader2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useKbImportJobDetail } from "@/hooks/useKbImportJob";
import { WIZARD_STEP_ID, type WizardStepId } from "./wizardSteps";

type Props = {
  stepId: WizardStepId;
  datapackName: string;
  isCreateFlow: boolean;
  backButtonLabel: string;
  onBack: () => void;
  /** Install step: no Back to previous wizard step. */
  showBack: boolean;
  /** Configure */
  configureSubmitting: boolean;
  canProceedFromConfigure: boolean;
  onConfigureSubmit: () => void;
  /** Add Knowledge (sync) — staged row counts */
  syncKnowledgeCount: number;
  syncConversationsCount: number;
  syncSkillsCount: number;
  canImport: boolean;
  onImport: () => void;
  /** Install: skip to sync; Sync: skip to import */
  onSkipForward: () => void;
  /** Finish (import) — uploaded counts for summary line */
  importKnowledgeCount: number;
  importConversationsCount: number;
  importSkillsCount: number;
  hasUploadedKnowledge: boolean;
  onFinish: () => void;
};

function CountsRow({
  knowledgeCount,
  conversationsCount,
  skillsCount,
  importStep,
  total,
  hasUploadedKnowledge,
}: {
  knowledgeCount: number;
  conversationsCount: number;
  skillsCount: number;
  importStep: boolean;
  total: number;
  hasUploadedKnowledge: boolean;
}) {
  return (
    <div className="min-w-0 flex-1 text-center text-xs text-muted-foreground sm:text-left">
      <span className="font-medium text-emerald-400">{knowledgeCount}</span> knowledge file{knowledgeCount === 1 ? "" : "s"}
      <span className="text-muted-foreground">, </span>
      <span className="font-medium text-sky-400">{conversationsCount}</span> conversation
      {conversationsCount === 1 ? "" : "s"}
      <span className="text-muted-foreground">, </span>
      <span className="font-medium text-purple-400">{skillsCount}</span> skill{skillsCount === 1 ? "" : "s"}
      {importStep ? (
        total === 0 ? (
          <span className="block pt-1 text-muted-foreground/80">
            No files staged — Finish will complete setup without creating a knowledge base.
          </span>
        ) : !hasUploadedKnowledge ? (
          <span className="block pt-1 text-muted-foreground/80">
            Upload files on the previous step, or Finish to skip knowledge base creation.
          </span>
        ) : null
      ) : null}
    </div>
  );
}

export function ConfigureWizardUnifiedFooter({
  stepId,
  datapackName,
  isCreateFlow,
  backButtonLabel,
  onBack,
  showBack,
  configureSubmitting,
  canProceedFromConfigure,
  onConfigureSubmit,
  syncKnowledgeCount,
  syncConversationsCount,
  syncSkillsCount,
  canImport,
  onImport,
  onSkipForward,
  importKnowledgeCount,
  importConversationsCount,
  importSkillsCount,
  hasUploadedKnowledge,
  onFinish,
}: Props) {
  const kbJob = useKbImportJobDetail();
  const finishBusy = stepId === WIZARD_STEP_ID.import && (kbJob.phase === "creating" || kbJob.phase === "polling");

  const showCounts =
    stepId === WIZARD_STEP_ID.sync ||
    stepId === WIZARD_STEP_ID.import;

  const counts =
    stepId === WIZARD_STEP_ID.sync
      ? {
        k: syncKnowledgeCount,
        c: syncConversationsCount,
        s: syncSkillsCount,
      }
      : {
        k: importKnowledgeCount,
        c: importConversationsCount,
        s: importSkillsCount,
      };

  const total = counts.k + counts.c + counts.s;

  const configurePrimary = (
    <Button
      type="button"
      disabled={!canProceedFromConfigure || configureSubmitting}
      className="bg-emerald-600 font-semibold text-white shadow-sm hover:bg-emerald-500 disabled:opacity-60"
      onClick={onConfigureSubmit}
    >
      {configureSubmitting ? (
        <>
          <Loader2 className="mr-2 h-4 w-4 animate-spin" aria-hidden />
          Saving…
        </>
      ) : isCreateFlow ? (
        <>
          Create
          <ArrowRight className="ml-2 h-4 w-4" aria-hidden />
        </>
      ) : (
        <>
          Edit
          <ArrowRight className="ml-2 h-4 w-4" aria-hidden />
        </>
      )}
    </Button>
  );

  const importDisabled = !canImport;
  const importButton = (
    <Button
      type="button"
      disabled={importDisabled}
      className="bg-emerald-600 font-semibold text-white shadow-sm hover:bg-emerald-500 disabled:opacity-60"
      onClick={onImport}
    >
      Import
      <ArrowRight className="ml-2 h-4 w-4" aria-hidden />
    </Button>
  );

  const detectButton = (
    <Button
      type="button"
      disabled
      className="bg-emerald-600 font-semibold text-white shadow-sm hover:bg-emerald-500 disabled:opacity-60"
    >
      Detect
    </Button>
  );

  const finishButton = (
    <Button
      type="button"
      disabled={finishBusy}
      className="bg-emerald-600 font-semibold text-white shadow-sm hover:bg-emerald-500 disabled:opacity-50"
      onClick={onFinish}
    >
      {finishBusy ? "Finishing…" : "Finish"}
    </Button>
  );

  let rightCluster: ReactNode = null;

  if (stepId === WIZARD_STEP_ID.configure) {
    rightCluster = configurePrimary;
  } else if (stepId === WIZARD_STEP_ID.install) {
    rightCluster = (
      <div className="flex flex-wrap items-center justify-end gap-4">
        <button
          type="button"
          className="text-sm text-muted-foreground underline underline-offset-4 transition hover:text-foreground"
          onClick={onSkipForward}
        >
          Skip for now
        </button>
        <Tooltip>
          <TooltipTrigger asChild>
            <span className="inline-flex">{detectButton}</span>
          </TooltipTrigger>
          <TooltipContent side="top">Coming Soon</TooltipContent>
        </Tooltip>
      </div>
    );
  } else if (stepId === WIZARD_STEP_ID.sync) {
    rightCluster = (
      <div className="flex flex-wrap items-center justify-end gap-4">
        <button
          type="button"
          className="text-sm text-muted-foreground underline underline-offset-4 transition hover:text-foreground"
          onClick={onSkipForward}
        >
          Skip step
        </button>
        {showBack ? (
          <Button type="button" variant="outline" onClick={onBack}>
            {backButtonLabel}
          </Button>
        ) : null}

        {importDisabled ? (
          <Tooltip>
            <TooltipTrigger asChild>
              <span className="inline-flex">{importButton}</span>
            </TooltipTrigger>
            <TooltipContent side="top">
              Upload knowledge files successfully before continuing, or use Skip step.
            </TooltipContent>
          </Tooltip>
        ) : (
          importButton
        )}
      </div>
    );
  } else if (stepId === WIZARD_STEP_ID.import) {
    rightCluster = (
      <div className="flex flex-wrap items-center justify-end gap-4">
        {showBack ? (
          <Button type="button" variant="outline" onClick={onBack}>
            {backButtonLabel}
          </Button>
        ) : null}
        {finishBusy ? (
          <Tooltip>
            <TooltipTrigger asChild>
              <span className="inline-flex">{finishButton}</span>
            </TooltipTrigger>
            <TooltipContent side="top">
              Import is running. You can keep browsing; we will notify you when it finishes.
            </TooltipContent>
          </Tooltip>
        ) : (
          finishButton
        )}
      </div>
    );
  }

  return (
    <div className="sticky bottom-0 z-[1] mt-8 border-t border-border bg-background/95 py-4 backdrop-blur supports-[backdrop-filter]:bg-background/80">
      <div className="flex flex-col gap-4 rounded-xl border border-border bg-card/60 px-4 py-4 shadow-sm sm:flex-row sm:items-center sm:gap-0 sm:px-6">
        <div className="flex min-w-0 shrink-0 flex-col gap-1 sm:max-w-[min(40%,280px)]">
          <div className="text-[10px] font-semibold uppercase tracking-wide text-muted-foreground">
            Datapack name
          </div>
          <code className="block truncate text-sm font-semibold text-foreground">{datapackName || "—"}</code>
        </div>

        {showCounts ? (
          <>
            <div className="hidden h-10 w-px shrink-0 bg-border sm:mx-6 sm:block" aria-hidden />
            <CountsRow
              knowledgeCount={counts.k}
              conversationsCount={counts.c}
              skillsCount={counts.s}
              importStep={stepId === WIZARD_STEP_ID.import}
              total={total}
              hasUploadedKnowledge={hasUploadedKnowledge}
            />
          </>
        ) : (
          <div className="hidden flex-1 sm:block" aria-hidden />
        )}

        <div className="flex shrink-0 flex-wrap items-center justify-end gap-3 sm:ml-auto">
          {stepId === WIZARD_STEP_ID.configure ? (
            <>
              {showBack ? (
                <Button type="button" variant="outline" onClick={onBack}>
                  {backButtonLabel}
                </Button>
              ) : null}
              {rightCluster}
            </>
          ) : (
            rightCluster
          )}
        </div>
      </div>
    </div>
  );
}
