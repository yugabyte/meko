/**
 * Route `/datapacks/:name/edit`: edit name and description, then continue to configure wizard.
 */
import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Info, Loader2, Pencil } from "lucide-react";
import { Button } from "@/components/ui/button";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { DatapackNameDescriptionFields } from "@/features/datapacks/components/DatapackNameDescriptionFields";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { datapackDetailPath, DEFAULT_ACCOUNT_DATAPACK_NAME, ROUTES } from "@/app/routes/paths";
import { toast } from "@/hooks/use-toast";
import type { CurrentUser } from "@/lib/mekoApi";
import { getApiErrorMessage, getDatapack, updateDatapack } from "@/lib/mekoApi";

type Props = {
  datapackName: string;
  datapackId?: string;
  currentUser: CurrentUser | null;
};

export default function DatapackEditPage({ datapackName, datapackId, currentUser }: Props) {
  const navigate = useNavigate();
  const [loading, setLoading] = useState(true);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [initialName, setInitialName] = useState("");
  const [initialDescription, setInitialDescription] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const isDefaultDatapack = initialName === DEFAULT_ACCOUNT_DATAPACK_NAME;

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    void getDatapack(datapackName, { datapackId })
      .then((dp) => {
        if (cancelled) return;
        const n = dp.datapack_name ?? datapackName;
        const d = dp.description ?? "";
        setName(n);
        setDescription(d);
        setInitialName(n);
        setInitialDescription(d);
      })
      .catch((err) => {
        if (!cancelled) {
          setError(getApiErrorMessage(err, "Could not load datapack."));
        }
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [datapackName]);

  const handleCancel = () => {
    if (currentUser && !currentUser.has_completed_onboarding) {
      navigate(ROUTES.onboarding);
      return;
    }
    navigate(datapackDetailPath(datapackName));
  };

  const handleUpdate = async () => {
    const trimmedName = name.trim();
    if (!trimmedName) {
      setError("Enter a datapack name.");
      return;
    }
    setSubmitting(true);
    setError(null);
    const descTrim = description.trim();
    const initialDescTrim = initialDescription.trim();
      const dirty =
      (!isDefaultDatapack && trimmedName !== initialName) || descTrim !== initialDescTrim;
    try {
      if (dirty) {
        await updateDatapack(datapackName, {
          name: trimmedName,
          description: description,
        }, { datapackId });
        toast({
          title: "Datapack updated",
          description: trimmedName !== initialName
            ? `Renamed to "${trimmedName}" successfully.`
            : "Description updated successfully.",
        });
      }
      navigate(datapackDetailPath(trimmedName), { replace: true });
    } catch (err) {
      setError(getApiErrorMessage(err, "Failed to update datapack."));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="relative flex h-screen flex-1 flex-col overflow-y-auto bg-background text-foreground">
      {loading ? (
        <div
          className="absolute inset-0 z-40 flex flex-col items-center justify-center gap-3 bg-background/80 backdrop-blur-sm"
          role="status"
          aria-live="polite"
        >
          <Loader2 className="h-9 w-9 shrink-0 animate-spin text-blue-400" aria-hidden />
          <p className="text-sm text-muted-foreground">Loading datapack…</p>
        </div>
      ) : null}

      <AppWorkspaceHeader />

      <main className="mx-auto flex w-full max-w-6xl flex-1 flex-col space-y-8 p-8 pb-16">
        <div className="mx-auto w-full max-w-xl space-y-8">
          <DatapackBreadcrumbs
            items={[
              { label: "Datapacks", href: ROUTES.datapacks },
              { label: datapackName, href: datapackDetailPath(datapackName) },
              { label: "edit" },
            ]}
          />

          <div className="space-y-2">
            <div className="flex items-center gap-3">
              <Pencil className="h-7 w-7 shrink-0 text-blue-400" aria-hidden />
              <h1 className="text-2xl font-semibold tracking-tight">Edit datapack</h1>
            </div>
            <p className="text-sm leading-relaxed text-muted-foreground">
              Set the name and description, then continue to configure ingestion and tools.
            </p>
          </div>

          {isDefaultDatapack ? (
            <div className="flex items-start gap-2 rounded-lg border border-blue-500/30 bg-blue-500/10 px-3 py-2 text-xs text-blue-300">
              <Info className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
              <span>The default datapack cannot be renamed. You can still update its description.</span>
            </div>
          ) : null}

          <DatapackNameDescriptionFields
            name={name}
            description={description}
            onNameChange={setName}
            onDescriptionChange={setDescription}
            disabled={submitting || loading}
            nameDisabled={isDefaultDatapack}
            nameInputId="datapack-edit-page-name"
            descriptionInputId="datapack-edit-page-description"
          />

          {error ? (
            <div className="rounded-md border border-red-500/40 bg-red-500/10 px-3 py-2 text-sm text-red-200">
              {error}
            </div>
          ) : null}

          <div className="flex flex-wrap items-center justify-end gap-2 pt-2">
            <Button type="button" variant="outline" onClick={handleCancel} disabled={submitting}>
              Cancel
            </Button>
            <Button
              type="button"
              className="bg-white text-black hover:bg-gray-200"
              onClick={() => void handleUpdate()}
              disabled={submitting || loading}
            >
              {submitting ? "Saving…" : "Update"}
            </Button>
          </div>
        </div>
      </main>
    </div>
  );
}
