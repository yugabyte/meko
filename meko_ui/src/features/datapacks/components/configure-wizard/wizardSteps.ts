/**
 * Ordered wizard steps and helpers for `/datapacks/:name/configure`.
 * Step ids drive which panel renders and how the stepper marks progress.
 */
export const WIZARD_STEP_ID = {
  install: "install",
  sync: "sync",
  import: "import",
} as const;

export type WizardStepId = (typeof WIZARD_STEP_ID)[keyof typeof WIZARD_STEP_ID];

export const WIZARD_STEPS: readonly { id: WizardStepId; label: string }[] = [
  { id: WIZARD_STEP_ID.install, label: "Install" },
  { id: WIZARD_STEP_ID.sync, label: "Sync Knowledge" },
  { id: WIZARD_STEP_ID.import, label: "Import" },
];

export function wizardStepIndex(stepId: WizardStepId): number {
  return WIZARD_STEPS.findIndex((s) => s.id === stepId);
}
