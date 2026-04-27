/**
 * Route `/datapacks/:name/configure`: post-create wizard (install → sync knowledge → import).
 * Composes modular step components; keeps sync list state and navigation here so steps stay presentational.
 */
import { useCallback, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { DatapackConfigureWizardStepper } from "@/features/datapacks/components/configure-wizard/DatapackConfigureWizardStepper";
import { ConfigureWizardStatusStep } from "@/features/datapacks/components/configure-wizard/ConfigureWizardStatusStep";
import { ConfigureWizardInstallStep } from "@/features/datapacks/components/configure-wizard/ConfigureWizardInstallStep";
import { ConfigureWizardNavBar } from "@/features/datapacks/components/configure-wizard/ConfigureWizardNavBar";
import { ConfigureWizardSyncStep } from "@/features/datapacks/components/configure-wizard/ConfigureWizardSyncStep";
import type { ListedSource, SyncSectionId } from "@/features/datapacks/components/configure-wizard/types";
import {
  WIZARD_STEP_ID,
  WIZARD_STEPS,
  type WizardStepId,
  wizardStepIndex,
} from "@/features/datapacks/components/configure-wizard/wizardSteps";
import { datapackDetailPath, datapackNewPath, ROUTES } from "@/app/routes/paths";
import { confirmUpload, getApiErrorMessage, requestUploadURL } from "@/lib/mekoApi";
import {
  KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSIONS,
  filterFilesForKnowledgeUploadBatch,
} from "@/lib/knowledgeFileUploadRules";
import { startKnowledgeBaseImport } from "@/lib/knowledgeBaseImportJob";

const UPLOAD_TIMEOUT_MS = 30000;

function uploadFileToS3(
  url: string,
  file: File,
  onProgress: (pct: number) => void,
): Promise<void> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.timeout = UPLOAD_TIMEOUT_MS;
    xhr.open("PUT", url);
    xhr.setRequestHeader("Content-Type", file.type || "application/octet-stream");

    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) {
        const pct = Math.round((e.loaded / e.total) * 100);
        onProgress(pct);
      }
    };

    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) resolve();
      else reject(new Error(`${xhr.status} ${xhr.statusText}: ${xhr.responseText}`));
    };
    xhr.onerror = () => reject(new Error(`Network error (status=${xhr.status})`));
    xhr.ontimeout = () => reject(new Error(`Request timed out after ${UPLOAD_TIMEOUT_MS / 1000}s`));
    xhr.send(file);
  });
}

type Props = {
  datapackName: string;
};

export default function DatapackConfigureWizardPage({ datapackName }: Props) {
  const navigate = useNavigate();
  const location = useLocation();
  const descriptionFromState = (location.state as { description?: string } | null)?.description;

  const [stepId, setStepId] = useState<WizardStepId>(WIZARD_STEP_ID.install);
  const currentIndex = wizardStepIndex(stepId);

  const [knowledgeItems, setKnowledgeItems] = useState<ListedSource[]>([]);
  const [knowledgeUploading, setKnowledgeUploading] = useState(false);
  const [syncFileWarning, setSyncFileWarning] = useState<string | null>(null);
  const syncWarningTimer = useRef<ReturnType<typeof setTimeout>>(undefined);

  const knowledgeInputRef = useRef<HTMLInputElement>(null);
  const conversationInputRef = useRef<HTMLInputElement>(null);
  const skillsInputRef = useRef<HTMLInputElement>(null);

  const goDetail = () => navigate(datapackDetailPath(datapackName));

  const showSyncWarning = useCallback((msg: string) => {
    setSyncFileWarning(msg);
    clearTimeout(syncWarningTimer.current);
    syncWarningTimer.current = setTimeout(() => setSyncFileWarning(null), 6000);
  }, []);

  const fileEntry = (f: File, includeUploadMeta: boolean): ListedSource => ({
    id: `${f.name}-${f.size}-${globalThis.crypto?.randomUUID?.() ?? Math.random().toString(36).slice(2)}`,
    label: f.name,
    kind: "file",
    ...(includeUploadMeta ? { file: f, uploadStatus: "pending" as const } : {}),
  });

  const addFiles = (section: SyncSectionId, files: FileList | null) => {
    if (section !== "knowledge" || !files?.length) return;
    setKnowledgeItems((prev) => {
      const staging = prev.filter((it) => it.kind === "file" && it.uploadStatus !== "done").length;
      const { accepted, warningParts } = filterFilesForKnowledgeUploadBatch(Array.from(files), staging);
      if (warningParts.length > 0) {
        const text = warningParts.join(". ");
        setTimeout(() => showSyncWarning(text), 0);
      }
      if (accepted.length === 0) return prev;
      return [...prev, ...accepted.map((f) => fileEntry(f, true))];
    });
  };

  const removeItem = (section: SyncSectionId, id: string) => {
    if (section === "knowledge") setKnowledgeItems((p) => p.filter((x) => x.id !== id));
  };

  const retryKnowledgeUpload = useCallback((id: string) => {
    setKnowledgeItems((prev) =>
      prev.map((x) =>
        x.id === id && x.uploadStatus === "error"
          ? { ...x, uploadStatus: "pending", uploadProgress: undefined, uploadError: undefined }
          : x,
      ),
    );
  }, []);

  const handleUploadKnowledgeFiles = useCallback(async () => {
    setKnowledgeUploading(true);
    try {
      const pending = knowledgeItems.filter(
        (it) => it.kind === "file" && it.file && (it.uploadStatus === "pending" || it.uploadStatus === "error"),
      );
      for (const item of pending) {
        const file = item.file!;
        setKnowledgeItems((prev) =>
          prev.map((x) =>
            x.id === item.id
              ? { ...x, uploadStatus: "uploading", uploadProgress: 0, uploadError: undefined }
              : x,
          ),
        );
        try {
          const { url, key } = await requestUploadURL(
            datapackName,
            file.name,
            file.type || "application/octet-stream",
            file.size,
          );
          await uploadFileToS3(url, file, (pct) => {
            setKnowledgeItems((prev) =>
              prev.map((x) => (x.id === item.id ? { ...x, uploadProgress: pct } : x)),
            );
          });
          await confirmUpload(datapackName, key, file.name, file.size);
          setKnowledgeItems((prev) =>
            prev.map((x) => (x.id === item.id ? { ...x, uploadStatus: "done", uploadProgress: 100 } : x)),
          );
        } catch (err) {
          setKnowledgeItems((prev) =>
            prev.map((x) =>
              x.id === item.id
                ? {
                  ...x,
                  uploadStatus: "error",
                  uploadProgress: 0,
                  uploadError: getApiErrorMessage(err, "Upload failed."),
                }
                : x,
            ),
          );
        }
      }
    } finally {
      setKnowledgeUploading(false);
    }
  }, [datapackName, knowledgeItems]);

  const goNext = () => {
    const next = WIZARD_STEPS[currentIndex + 1];
    if (next) setStepId(next.id);
  };

  const goBack = () => {
    const prev = WIZARD_STEPS[currentIndex - 1];
    if (prev) setStepId(prev.id);
    else navigate(ROUTES.datapacks);
  };

  const canImportKnowledge = knowledgeItems.some((it) => it.uploadStatus === "done");

  const syncAllKnowledgeUploaded =
    knowledgeItems.length > 0 &&
    knowledgeItems.every((it) => it.kind === "file" && it.uploadStatus === "done");

  const canGoNextFromSync = stepId !== WIZARD_STEP_ID.sync || syncAllKnowledgeUploaded;

  return (
    <div className="flex h-screen flex-1 flex-col overflow-y-auto bg-background text-foreground">
      <AppWorkspaceHeader />

      <main className="mx-auto flex w-full max-w-6xl flex-1 flex-col space-y-8 p-8 pb-32">
        <div className="mx-auto w-full max-w-5xl space-y-8">
          <DatapackBreadcrumbs
            items={[
              { label: "Datapacks", href: ROUTES.datapacks },
              { label: "new", href: datapackNewPath() },
              { label: "configure" },
            ]}
          />

          <div className="space-y-2">
            <h1 className="text-2xl font-semibold tracking-tight sm:text-3xl">
              Configure <span className="text-foreground">{datapackName}</span>
            </h1>
            {descriptionFromState ? (
              <p className="max-w-3xl text-sm leading-relaxed text-muted-foreground">{descriptionFromState}</p>
            ) : (
              <p className="max-w-3xl text-sm leading-relaxed text-muted-foreground">
                Finish setup: install the client, add knowledge to sync, then review import.
              </p>
            )}
          </div>

          <div className="w-full min-w-0 rounded-xl border border-border bg-muted/20 py-4 sm:py-5">
            <div className="w-full min-w-0 px-2 sm:px-3">
              <DatapackConfigureWizardStepper currentIndex={currentIndex} />
            </div>
          </div>

          {stepId === WIZARD_STEP_ID.install ? <ConfigureWizardInstallStep /> : null}
          {stepId === WIZARD_STEP_ID.sync ? (
            <ConfigureWizardSyncStep
              knowledgeItems={knowledgeItems}
              conversationItems={[]}
              skillItems={[]}
              knowledgeInputRef={knowledgeInputRef}
              conversationInputRef={conversationInputRef}
              skillsInputRef={skillsInputRef}
              onPickFiles={addFiles}
              onAddUrl={() => { }}
              onRemove={removeItem}
              knowledgeUploading={knowledgeUploading}
              onUploadKnowledgeFiles={handleUploadKnowledgeFiles}
              onRetryKnowledgeUpload={retryKnowledgeUpload}
              knowledgeFileAccept={KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSIONS}
              syncFileWarning={syncFileWarning}
            />
          ) : null}
          {stepId === WIZARD_STEP_ID.import ? (
            <ConfigureWizardStatusStep
              datapackName={datapackName}
              knowledgeCount={knowledgeItems.length}
              conversationsCount={0}
              skillsCount={0}
              canImportKnowledge={canImportKnowledge}
              onSkip={goDetail}
              onStartImport={() => startKnowledgeBaseImport(datapackName)}
            />
          ) : null}

          <ConfigureWizardNavBar
            stepId={stepId}
            currentIndex={currentIndex}
            onBack={goBack}
            onNext={goNext}
            onOpenDatapack={goDetail}
            canGoNext={canGoNextFromSync}
            nextDisabledTooltip="Upload knowledge file successfully before continuing to import."
          />
        </div>
      </main>
    </div>
  );
}
