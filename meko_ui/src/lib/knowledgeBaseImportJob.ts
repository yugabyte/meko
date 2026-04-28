/**
 * Fire-and-forget knowledge base creation + pipeline polling for the configure wizard import step.
 * Survives route changes; drives {@link KB_IMPORT_JOB_EVENT} for the global banner.
 */
import { dispatchAppEvent } from "@/lib/browserEvents";
import { createKnowledgeBase, getApiErrorMessage, getKBStatus } from "@/lib/mekoApi";
import { markDatapackWizardConfigureDone } from "@/lib/datapackConfigureStatus";
import { toast } from "@/hooks/use-toast";

export const KB_IMPORT_JOB_EVENT = "meko:kb-import-job";

export type KbImportJobPhase = "idle" | "creating" | "polling" | "success" | "error";

export type KbImportJobDetail = {
  phase: KbImportJobPhase;
  datapackName?: string;
  message?: string;
};

const POLL_MS = 5000;
const MAX_WAIT_MS = 15 * 60 * 1000;
let pollGraceTicks = 0;

function emit(detail: KbImportJobDetail) {
  dispatchAppEvent(KB_IMPORT_JOB_EVENT, detail);
}

async function pollUntilKbSettled(datapackName: string): Promise<void> {
  const deadline = Date.now() + MAX_WAIT_MS;
  pollGraceTicks = 3;

  while (Date.now() < deadline) {
    try {
      const status = await getKBStatus(datapackName);
      const sources = status.source_statuses ?? [];
      const details = status.pipeline_details ?? [];

      const sourcesReady =
        sources.length > 0 && sources.every((s) => s.status === "COMPLETED");
      const allDocsDone =
        details.length > 0 &&
        details.every((d) => d.document_status === "COMPLETED" || d.document_status === "FAILED");

      if (pollGraceTicks > 0) {
        pollGraceTicks--;
      } else if (sourcesReady && allDocsDone) {
        return;
      }
    } catch {
      // keep polling
    }
    await new Promise((r) => setTimeout(r, POLL_MS));
  }
  throw new Error("Import is taking longer than expected. Check the datapack page for status.");
}

let inFlight = false;

export function isKnowledgeBaseImportInFlight(): boolean {
  return inFlight;
}

export function startKnowledgeBaseImport(datapackName: string): void {
  const name = datapackName.trim();
  if (!name || inFlight) return;

  inFlight = true;
  emit({ phase: "creating", datapackName: name });

  void (async () => {
    try {
      await createKnowledgeBase(name);
      markDatapackWizardConfigureDone(name);
      emit({ phase: "polling", datapackName: name });
      await pollUntilKbSettled(name);
      emit({ phase: "success", datapackName: name });
      toast({
        title: "Import complete",
        description: `Imported knowledge to ${name} successfully.`,
      });
    } catch (err) {
      const message = getApiErrorMessage(err, "Knowledge import failed.");
      emit({ phase: "error", datapackName: name, message });
      toast({
        variant: "destructive",
        title: "Import failed",
        description: message,
      });
    } finally {
      inFlight = false;
      emit({ phase: "idle" });
    }
  })();
}
