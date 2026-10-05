/**
 * Wizard step 1: datapack name and description (create or edit existing).
 */
import { Loader2, Pencil } from "lucide-react";
import { DatapackNameDescriptionFields } from "@/features/datapacks/components/DatapackNameDescriptionFields";
import { ProjectFolderSelect } from "@/features/projects/components/ProjectFolderSelect";

type Props = {
  isCreateFlow: boolean;
  name: string;
  description: string;
  onNameChange: (v: string) => void;
  onDescriptionChange: (v: string) => void;
  loading: boolean;
  submitting: boolean;
  error: string | null;
  /** Create flow only, and only with Projects enabled: a new datapack is
   *  created inside a folder. */
  showFolderPicker?: boolean;
  folderId?: string;
  onFolderChange?: (folderId: string) => void;
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
  showFolderPicker = false,
  folderId = "",
  onFolderChange,
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
          <Pencil className="h-7 w-7 shrink-0 text-brand" aria-hidden />
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

      {showFolderPicker && onFolderChange ? (
        <div className="space-y-4 rounded-xl border border-border bg-card/40 p-6 shadow-sm">
          <p className="text-sm text-muted-foreground">
            Optionally file this datapack into a project folder. Leave it unset and the datapack
            is created unfiled, which works exactly as it always has; you can file or move it at
            any time afterwards.
          </p>
          <ProjectFolderSelect
            folderId={folderId}
            onFolderChange={onFolderChange}
            enabled={!busy}
            idPrefix="wizard-configure"
          />
        </div>
      ) : null}

      {error ? (
        <div className="rounded-md border border-red-500/40 bg-red-500/10 px-3 py-2 text-sm text-red-700 dark:text-red-200">{error}</div>
      ) : null}
    </div>
  );
}
