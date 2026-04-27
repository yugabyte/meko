/**
 * Configure wizard step: placeholder copy before Sync Data (memory verification is not wired yet).
 * Keeps layout aligned with other bordered step panels.
 */
export function ConfigureWizardDetectStep() {
  return (
    <div className="rounded-xl border border-border bg-card/30 p-6 sm:p-8">
      <p className="max-w-2xl text-sm leading-relaxed text-muted-foreground">
        We will test memory for this datapack once your environment is connected. Continue when you are ready to add
        sources on the next step, then review status when you are done.
      </p>
    </div>
  );
}
