/**
 * Wizard step 1: datapack name and description (create or edit existing).
 */
import { Loader2, Pencil } from "lucide-react";
import { DatapackNameDescriptionFields } from "@/features/datapacks/components/DatapackNameDescriptionFields";

type Props = {
  isCreateFlow: boolean;
  name: string;
  description: string;
  onNameChange: (v: string) => void;
  onDescriptionChange: (v: string) => void;
  loading: boolean;
  submitting: boolean;
  error: string | null;
};

export function ConfigureWizardConfigureStep({
  isCreateFlow,
  name,
  description,
  onNameChange,
  onDescriptionChange,
  loading,
  submitting,
  error,
}: Props) {
  const busy = loading || submitting;

  return (
    <div className="relative space-y-6">
      {loading ? (
        <div
          className="absolute inset-0 z-10 flex flex-col items-center justify-center gap-3 rounded-xl bg-background/80 backdrop-blur-sm"
          role="status"
          aria-live="polite"
        >
          <Loader2 className="h-9 w-9 shrink-0 animate-spin text-blue-400" aria-hidden />
          <p className="text-sm text-muted-foreground">Loading datapack…</p>
        </div>
      ) : null}

      <div className="space-y-2">
        <div className="flex items-center gap-3">
          <Pencil className="h-7 w-7 shrink-0 text-blue-400" aria-hidden />
          <h2 className="text-xl font-semibold tracking-tight">
            {isCreateFlow ? "Name your datapack" : "Datapack details"}
          </h2>
        </div>
        <p className="max-w-3xl text-sm leading-relaxed text-muted-foreground">
          {isCreateFlow
            ? "Choose a unique name and optional description. On Next we create the datapack, then you continue with install and knowledge."
            : "Update the name or description if needed, then continue to install and knowledge steps."}
        </p>
      </div>

      <DatapackNameDescriptionFields
        name={name}
        description={description}
        onNameChange={onNameChange}
        onDescriptionChange={onDescriptionChange}
        disabled={busy}
        nameInputId="wizard-configure-datapack-name"
        descriptionInputId="wizard-configure-datapack-description"
      />

      {error ? (
        <div className="rounded-md border border-red-500/40 bg-red-500/10 px-3 py-2 text-sm text-red-200">{error}</div>
      ) : null}
    </div>
  );
}
