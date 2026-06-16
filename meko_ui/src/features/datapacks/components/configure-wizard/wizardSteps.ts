/**
 * Ordered wizard steps and helpers for `/datapacks/:datapack_id/configure`.
 * Step ids drive which panel renders and how the stepper marks progress.
 */
export const WIZARD_STEP_ID = {
  configure: "configure",
  install: "install",
  sync: "sync",
  import: "import",
} as const;

export type WizardStepId = (typeof WIZARD_STEP_ID)[keyof typeof WIZARD_STEP_ID];

export const WIZARD_STEPS: readonly { id: WizardStepId; label: string }[] = [
  { id: WIZARD_STEP_ID.configure, label: "Configure" },
  { id: WIZARD_STEP_ID.install, label: "Connect" },
  { id: WIZARD_STEP_ID.sync, label: "Add Knowledge" },
  { id: WIZARD_STEP_ID.import, label: "Finish" },
];

export function wizardStepIndex(stepId: WizardStepId): number {
  return WIZARD_STEPS.findIndex((s) => s.id === stepId);
}
