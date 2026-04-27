/**
 * Route `/datapacks/new`: collect name and optional description, then create a datapack with an empty seed URL.
 * On success, navigates to the configure wizard; full-screen overlay covers the page while create is in flight.
 */
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { Loader2, Plus } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { createDatapack, getApiErrorMessage } from "@/lib/mekoApi";
import { datapackConfigurePath, ROUTES } from "@/app/routes/paths";

export default function DatapackNewPage() {
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [isSubmitting, setIsSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleCancel = () => {
    navigate(ROUTES.datapacks);
  };

  const handleCreate = async () => {
    const trimmed = name.trim();
    if (!trimmed) {
      setError("Enter a datapack name.");
      return;
    }
    setIsSubmitting(true);
    setError(null);
    try {
      const created = await createDatapack({
        name: trimmed,
        seed_knowledge_base: "",
      });
      if (created.mcp_server?.api_key) {
        localStorage.setItem("meko_latest_api_key", created.mcp_server.api_key);
      }
      navigate(datapackConfigurePath(created.name), {
        state: { description: description.trim() || undefined },
      });
    } catch (err) {
      setError(getApiErrorMessage(err, "Failed to create datapack."));
    } finally {
      setIsSubmitting(false);
    }
  };

  return (
    <div className="relative flex h-screen flex-1 flex-col overflow-y-auto bg-background text-foreground">
      {/* Blocks interaction and signals slow provision while createDatapack runs. */}
      {isSubmitting ? (
        <div
          className="absolute inset-0 z-50 flex flex-col items-center justify-center gap-4 bg-background/85 backdrop-blur-sm"
          role="status"
          aria-live="polite"
          aria-busy="true"
        >
          <Loader2 className="h-10 w-10 shrink-0 animate-spin text-blue-400" aria-hidden />
          <p className="text-sm text-muted-foreground">Creating datapack…</p>
        </div>
      ) : null}

      <AppWorkspaceHeader />

      <main className="mx-auto flex w-full max-w-6xl flex-1 flex-col space-y-8 p-8 pb-16">
        <div className="mx-auto w-full max-w-xl space-y-8">
          <DatapackBreadcrumbs
            items={[
              { label: "Datapacks", href: ROUTES.datapacks },
              { label: "new" },
            ]}
          />

          <div className="space-y-2">
            <div className="flex items-center gap-3">
              <Plus className="h-7 w-7 shrink-0 text-blue-400" aria-hidden />
              <h1 className="text-2xl font-semibold tracking-tight">New datapack</h1>
            </div>
            <p className="text-sm leading-relaxed text-muted-foreground">
              Choose a unique name and an optional description. You can configure ingestion after it is created.
            </p>
          </div>

          <div className="space-y-6 rounded-xl border border-border bg-card/40 p-6 shadow-sm">
            <div className="flex flex-col gap-3">
              <Label htmlFor="datapack-name">Name</Label>
              <Input
                id="datapack-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="e.g. foo-project"
                autoComplete="off"
                disabled={isSubmitting}
                className="h-10 border-border bg-background shadow-sm"
              />
            </div>
            <div className="flex flex-col gap-3">
              <Label htmlFor="datapack-description">Description</Label>
              <Textarea
                id="datapack-description"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                placeholder="What this datapack is for (optional)"
                rows={4}
                disabled={isSubmitting}
                className="min-h-[100px] resize-y border-border bg-background shadow-sm"
              />
            </div>

            {error ? (
              <div className="rounded-md border border-red-500/40 bg-red-500/10 px-3 py-2 text-sm text-red-200">
                {error}
              </div>
            ) : null}

            <div className="flex flex-wrap items-center justify-end gap-2 pt-2">
              <Button type="button" variant="outline" onClick={handleCancel} disabled={isSubmitting}>
                Cancel
              </Button>
              <Button
                type="button"
                className="bg-white text-black hover:bg-gray-200"
                onClick={() => void handleCreate()}
                disabled={isSubmitting}
              >
                Create
              </Button>
            </div>
          </div>
        </div>
      </main>
    </div>
  );
}
