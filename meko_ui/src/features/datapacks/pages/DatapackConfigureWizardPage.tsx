/**
 * Configure wizard:
 * - `/datapacks/new/configure` — create flow (Configure creates datapack, then Install → Add Knowledge → Finish).
 * - `/datapacks/:name/configure` — existing datapack (same steps; Configure can be skipped).
 */
import { useCallback, useEffect, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { notifyDatapacksListChanged } from "@/features/observe/observeRefresh";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { DatapackConfigureWizardStepper } from "@/features/datapacks/components/configure-wizard/DatapackConfigureWizardStepper";
import { ConfigureWizardConfigureStep } from "@/features/datapacks/components/configure-wizard/ConfigureWizardConfigureStep";
import { ConfigureWizardStatusStep } from "@/features/datapacks/components/configure-wizard/ConfigureWizardStatusStep";
import { ConfigureWizardInstallStep } from "@/features/datapacks/components/configure-wizard/ConfigureWizardInstallStep";
import { ConfigureWizardUnifiedFooter } from "@/features/datapacks/components/configure-wizard/ConfigureWizardUnifiedFooter";
import { ConfigureWizardSyncStep } from "@/features/datapacks/components/configure-wizard/ConfigureWizardSyncStep";
import { ConfigureWizardPageHeader } from "@/features/datapacks/components/configure-wizard/ConfigureWizardPageHeader";
import type { ListedSource, SyncSectionId } from "@/features/datapacks/components/configure-wizard/types";
import {
  WIZARD_STEP_ID,
  WIZARD_STEPS,
  type WizardStepId,
  wizardStepIndex,
} from "@/features/datapacks/components/configure-wizard/wizardSteps";
import { datapackDetailPath, ROUTES } from "@/app/routes/paths";
import {
  confirmUpload,
  createDatapack,
  getApiErrorMessage,
  getDatapack,
  getDatapackProvisionStatus,
  requestUploadURL,
  updateDatapack,
  type CurrentUser,
  type ProvisionStatus,
} from "@/lib/mekoApi";
import {
  KNOWLEDGE_UPLOAD_ACCEPTED_EXTENSIONS,
  filterDuplicateKnowledgeFileNames,
  filterFilesForKnowledgeUploadBatch,
} from "@/lib/knowledgeFileUploadRules";
import { startKnowledgeBaseImport } from "@/lib/knowledgeBaseImportJob";
import { KNOWLEDGE_UPLOAD_STATUS } from "@/lib/knowledgeUploadStatus";
import { toast } from "@/hooks/use-toast";

const UPLOAD_TIMEOUT_MS = 30000;
const PERSISTED_KNOWLEDGE_FILES_PREFIX = "meko:configure-wizard:knowledge-files:v1:";

type PersistedKnowledgeFile = {
  label: string;
  sizeBytes: number;
};

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

function isDuplicateUploadError(message: string): boolean {
  const normalized = message.toLowerCase();
  return (
    normalized.includes("duplicate_file") ||
    normalized.includes("same name") ||
    normalized.includes("already exists")
  );
}

type DatapackConfigureWizardPageProps = {
  currentUser: CurrentUser | null;
  /** `/datapacks/new/configure` */
  isCreateFlow?: boolean;
  /** `/datapacks/:name/configure` */
  datapackName?: string;
  datapackId?: string;
};

export default function DatapackConfigureWizardPage({
  currentUser,
  isCreateFlow = false,
  datapackName: routeDatapackName,
  datapackId: routeDatapackId,
}: DatapackConfigureWizardPageProps) {
  const navigate = useNavigate();
  const location = useLocation();
  const descriptionFromState = (location.state as { description?: string } | null)?.description;

  const [stepId, setStepId] = useState<WizardStepId>(WIZARD_STEP_ID.configure);
  const currentIndex = wizardStepIndex(stepId);

  /** Resolved server name for API calls (set after create, or from route / load for existing). */
  const [apiDatapackName, setApiDatapackName] = useState<string | null>(() =>
    isCreateFlow ? null : routeDatapackName ?? null,
  );

  const [configureDraftName, setConfigureDraftName] = useState("");
  const [configureDraftDescription, setConfigureDraftDescription] = useState(() => descriptionFromState ?? "");
  const [configureInitialName, setConfigureInitialName] = useState("");
  const [configureInitialDescription, setConfigureInitialDescription] = useState("");
  const [configureLoading, setConfigureLoading] = useState(!isCreateFlow);
  const [configureError, setConfigureError] = useState<string | null>(null);
  const [configureSubmitting, setConfigureSubmitting] = useState(false);

  const [knowledgeItems, setKnowledgeItems] = useState<ListedSource[]>([]);
  const [knowledgeUploading, setKnowledgeUploading] = useState(false);
  const [syncFileWarning, setSyncFileWarning] = useState<string | null>(null);
  const syncWarningTimer = useRef<ReturnType<typeof setTimeout>>(undefined);

  const [installProvision, setInstallProvision] = useState<ProvisionStatus | null>(null);
  const [installProvisionLoading, setInstallProvisionLoading] = useState(false);
  const [installProvisionError, setInstallProvisionError] = useState<string | null>(null);

  const knowledgeInputRef = useRef<HTMLInputElement>(null);
  const conversationInputRef = useRef<HTMLInputElement>(null);
  const skillsInputRef = useRef<HTMLInputElement>(null);
  const restoredPersistenceRef = useRef<string | null>(null);

  const persistedKnowledgeStorageKey = (() => {
    const identity = routeDatapackId ?? apiDatapackName ?? routeDatapackName;
    return identity ? `${PERSISTED_KNOWLEDGE_FILES_PREFIX}${identity}` : null;
  })();

  useEffect(() => {
    if (isCreateFlow) {
      setConfigureLoading(false);
      setApiDatapackName(null);
      return;
    }
    if (!routeDatapackName) return;
    let cancelled = false;
    setConfigureLoading(true);
    setConfigureError(null);
    void getDatapack(routeDatapackName, { datapackId: routeDatapackId })
      .then((dp) => {
        if (cancelled) return;
        const n = dp.datapack_name ?? routeDatapackName;
        const d = dp.description ?? "";
        setConfigureDraftName(n);
        setConfigureDraftDescription(d);
        setConfigureInitialName(n);
        setConfigureInitialDescription(d);
        setApiDatapackName(n);
      })
      .catch((err) => {
        if (!cancelled) setConfigureError(getApiErrorMessage(err, "Could not load datapack."));
      })
      .finally(() => {
        if (!cancelled) setConfigureLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [isCreateFlow, routeDatapackName]);

  useEffect(() => {
    if (stepId !== WIZARD_STEP_ID.install || !apiDatapackName) {
      return;
    }
    const ac = new AbortController();
    setInstallProvisionLoading(true);
    setInstallProvisionError(null);
    setInstallProvision(null);
    void getDatapackProvisionStatus(apiDatapackName, { signal: ac.signal })
      .then((p) => {
        if (!ac.signal.aborted) setInstallProvision(p);
      })
      .catch((err) => {
        if (ac.signal.aborted) return;
        setInstallProvisionError(
          getApiErrorMessage(err, "Could not load MCP connection details for install commands."),
        );
      })
      .finally(() => {
        if (!ac.signal.aborted) setInstallProvisionLoading(false);
      });
    return () => ac.abort();
  }, [stepId, apiDatapackName]);

  const showSyncWarning = useCallback((msg: string) => {
    setSyncFileWarning(msg);
    clearTimeout(syncWarningTimer.current);
    syncWarningTimer.current = setTimeout(() => setSyncFileWarning(null), 6000);
  }, []);

  const fileEntry = (f: File, includeUploadMeta: boolean): ListedSource => ({
    id: `${f.name}-${f.size}-${globalThis.crypto?.randomUUID?.() ?? Math.random().toString(36).slice(2)}`,
    label: f.name,
    kind: "file",
    sizeBytes: f.size,
    ...(includeUploadMeta ? { file: f, uploadStatus: KNOWLEDGE_UPLOAD_STATUS.PENDING } : {}),
  });

  useEffect(() => {
    if (!persistedKnowledgeStorageKey) return;
    if (restoredPersistenceRef.current === persistedKnowledgeStorageKey) return;
    restoredPersistenceRef.current = persistedKnowledgeStorageKey;

    try {
      const raw = localStorage.getItem(persistedKnowledgeStorageKey);
      if (!raw) return;
      const parsed = JSON.parse(raw) as PersistedKnowledgeFile[];
      if (!Array.isArray(parsed) || parsed.length === 0) return;

      setKnowledgeItems((prev) => {
        const existing = new Set(prev.map((it) => it.label.toLowerCase()));
        const restored: ListedSource[] = parsed
          .filter((it) => it?.label && Number.isFinite(it.sizeBytes) && it.sizeBytes >= 0)
          .filter((it) => !existing.has(it.label.toLowerCase()))
          .map((it) => ({
            id: `persisted-${it.label}-${it.sizeBytes}`,
            label: it.label,
            kind: "file",
            sizeBytes: it.sizeBytes,
            uploadStatus: KNOWLEDGE_UPLOAD_STATUS.DONE,
            uploadProgress: 100,
          }));

        if (restored.length === 0) return prev;
        return [...prev, ...restored];
      });
    } catch {
      // Ignore malformed persistence payloads.
    }
  }, [persistedKnowledgeStorageKey]);

  useEffect(() => {
    if (!persistedKnowledgeStorageKey) return;
    try {
      const doneFiles: PersistedKnowledgeFile[] = knowledgeItems
        .filter((it) => it.kind === "file" && it.uploadStatus === KNOWLEDGE_UPLOAD_STATUS.DONE)
        .map((it) => ({
          label: it.label,
          sizeBytes: it.file?.size ?? it.sizeBytes ?? 0,
        }))
        .filter((it) => it.sizeBytes >= 0);

      if (doneFiles.length === 0) {
        localStorage.removeItem(persistedKnowledgeStorageKey);
      } else {
        localStorage.setItem(persistedKnowledgeStorageKey, JSON.stringify(doneFiles));
      }
    } catch {
      // Ignore storage errors.
    }
  }, [knowledgeItems, persistedKnowledgeStorageKey]);

  const addFiles = (section: SyncSectionId, files: FileList | null) => {
    if (section !== "knowledge" || !files?.length) return;
    setKnowledgeItems((prev) => {
      const staging = prev.filter((it) => it.kind === "file" && it.uploadStatus !== KNOWLEDGE_UPLOAD_STATUS.DONE)
        .length;
      const { accepted, warningParts } = filterFilesForKnowledgeUploadBatch(Array.from(files), staging);
      const existingNames = prev.filter((it) => it.kind === "file").map((it) => it.label);
      const { unique, duplicateCount } = filterDuplicateKnowledgeFileNames(accepted, existingNames);
      const warnings = [...warningParts];
      if (duplicateCount > 0) {
        warnings.push(
          `${duplicateCount} skipped (same filename already in the list or duplicated in this selection)`,
        );
      }
      if (warnings.length > 0) {
        setTimeout(() => showSyncWarning(warnings.join(". ")), 0);
      }
      if (unique.length === 0) return prev;
      return [...prev, ...unique.map((f) => fileEntry(f, true))];
    });
  };

  const removeItem = (section: SyncSectionId, id: string) => {
    if (section === "knowledge") setKnowledgeItems((p) => p.filter((x) => x.id !== id));
  };

  const retryKnowledgeUpload = useCallback((id: string) => {
    setKnowledgeItems((prev) =>
      prev.map((x) =>
        x.id === id && x.uploadStatus === KNOWLEDGE_UPLOAD_STATUS.ERROR
          ? {
            ...x,
            uploadStatus: KNOWLEDGE_UPLOAD_STATUS.PENDING,
            uploadProgress: undefined,
            uploadError: undefined,
          }
          : x,
      ),
    );
  }, []);

  const handleUploadKnowledgeFiles = useCallback(async (includedIds: Set<string>) => {
    const pack = apiDatapackName;
    if (!pack) return;
    setKnowledgeUploading(true);
    try {
      const pending = knowledgeItems.filter(
        (it) =>
          it.kind === "file" &&
          it.file &&
          includedIds.has(it.id) &&
          (it.uploadStatus === KNOWLEDGE_UPLOAD_STATUS.PENDING ||
            it.uploadStatus === KNOWLEDGE_UPLOAD_STATUS.ERROR),
      );
      for (const item of pending) {
        const file = item.file!;
        setKnowledgeItems((prev) =>
          prev.map((x) =>
            x.id === item.id
              ? { ...x, uploadStatus: KNOWLEDGE_UPLOAD_STATUS.UPLOADING, uploadProgress: 0, uploadError: undefined }
              : x,
          ),
        );
        try {
          const { url, key } = await requestUploadURL(
            pack,
            file.name,
            file.type || "application/octet-stream",
            file.size,
          );
          await uploadFileToS3(url, file, (pct) => {
            setKnowledgeItems((prev) =>
              prev.map((x) => (x.id === item.id ? { ...x, uploadProgress: pct } : x)),
            );
          });
          await confirmUpload(pack, key, file.name, file.size);
          setKnowledgeItems((prev) =>
            prev.map((x) =>
              x.id === item.id ? { ...x, uploadStatus: KNOWLEDGE_UPLOAD_STATUS.DONE, uploadProgress: 100 } : x,
            ),
          );
        } catch (err) {
          const message = getApiErrorMessage(err, "Upload failed.");
          if (isDuplicateUploadError(message)) {
            setKnowledgeItems((prev) =>
              prev.map((x) =>
                x.id === item.id
                  ? {
                    ...x,
                    uploadStatus: KNOWLEDGE_UPLOAD_STATUS.DONE,
                    uploadProgress: 100,
                    uploadError: undefined,
                  }
                  : x,
              ),
            );
            continue;
          }
          setKnowledgeItems((prev) =>
            prev.map((x) =>
              x.id === item.id
                ? {
                  ...x,
                  uploadStatus: KNOWLEDGE_UPLOAD_STATUS.ERROR,
                  uploadProgress: 0,
                  uploadError: message,
                }
                : x,
            ),
          );
        }
      }
    } finally {
      setKnowledgeUploading(false);
    }
  }, [apiDatapackName, knowledgeItems]);

  const syncAllKnowledgeUploaded =
    knowledgeItems.length > 0 &&
    knowledgeItems.every((it) => it.kind === "file" && it.uploadStatus === KNOWLEDGE_UPLOAD_STATUS.DONE);

  const handleConfigureStepNext = useCallback(async () => {
    const trimmed = configureDraftName.trim();
    if (!trimmed) {
      setConfigureError("Enter a datapack name.");
      return;
    }
    if (!currentUser?.account_id) {
      setConfigureError("You must be signed in to continue.");
      return;
    }
    setConfigureSubmitting(true);
    setConfigureError(null);
    const descTrim = configureDraftDescription.trim();
    try {
      if (isCreateFlow) {
        if (apiDatapackName) {
          const dirty =
            trimmed !== configureInitialName ||
            configureDraftDescription.trim() !== configureInitialDescription.trim();
          if (dirty) {
            await updateDatapack(apiDatapackName, {
              name: trimmed,
              description: configureDraftDescription,
            }, { datapackId: routeDatapackId });
            setApiDatapackName(trimmed);
            setConfigureInitialName(trimmed);
            setConfigureInitialDescription(configureDraftDescription);
          }
          setStepId(WIZARD_STEP_ID.install);
          return;
        }
        const created = await createDatapack({
          name: trimmed,
          user_id: currentUser.account_id,
          ...(descTrim ? { description: configureDraftDescription } : {}),
        });
        if (created?.name) {
          notifyDatapacksListChanged(`create:${created.name}`);
        }
        if (created.mcp_server?.api_key) {
          localStorage.setItem("meko_latest_api_key", created.mcp_server.api_key);
        }
        const resolved = created.name;
        setApiDatapackName(resolved);
        setConfigureDraftName(resolved);
        setConfigureInitialName(resolved);
        setConfigureInitialDescription(configureDraftDescription);
        setStepId(WIZARD_STEP_ID.install);
        return;
      }
      const routeName = routeDatapackName;
      if (!routeName) return;
      const dirty =
        trimmed !== configureInitialName ||
        configureDraftDescription.trim() !== configureInitialDescription.trim();
      if (dirty) {
        const updated = await updateDatapack(routeName, {
          name: trimmed,
          description: configureDraftDescription,
        }, { datapackId: routeDatapackId });
        if (updated?.message) {
          notifyDatapacksListChanged(`rename:${routeName}->${trimmed}`);
        }
        setApiDatapackName(trimmed);
        setConfigureInitialName(trimmed);
        setConfigureInitialDescription(configureDraftDescription);
      }
      setStepId(WIZARD_STEP_ID.install);
    } catch (err) {
      setConfigureError(getApiErrorMessage(err, "Failed to save datapack."));
    } finally {
      setConfigureSubmitting(false);
    }
  }, [
    apiDatapackName,
    configureDraftDescription,
    configureDraftName,
    configureInitialDescription,
    configureInitialName,
    currentUser?.account_id,
    isCreateFlow,
    routeDatapackName,
  ]);

  const handleWizardNext = useCallback(async () => {
    if (stepId === WIZARD_STEP_ID.configure) {
      await handleConfigureStepNext();
      return;
    }
    if (stepId === WIZARD_STEP_ID.install) {
      setStepId(WIZARD_STEP_ID.sync);
      return;
    }
    if (stepId === WIZARD_STEP_ID.sync && syncAllKnowledgeUploaded) {
      setStepId(WIZARD_STEP_ID.import);
    }
  }, [handleConfigureStepNext, stepId, syncAllKnowledgeUploaded]);

  const goBack = () => {
    if (currentIndex === 0) {
      if (isCreateFlow) {
        navigate(ROUTES.datapacks);
      } else if (apiDatapackName ?? routeDatapackName) {
        navigate(datapackDetailPath(apiDatapackName ?? routeDatapackName ?? "", routeDatapackId));
      } else {
        navigate(ROUTES.datapacks);
      }
      return;
    }
    const prev = WIZARD_STEPS[currentIndex - 1];
    if (prev) setStepId(prev.id);
  };

  const backButtonLabel =
    currentIndex === 0 ? (isCreateFlow ? "Back to datapacks" : "Back") : "Back";

  const hasUploadedKnowledge = knowledgeItems.some((it) => it.uploadStatus === KNOWLEDGE_UPLOAD_STATUS.DONE);
  const knowledgeDoneCount = knowledgeItems.filter(
    (it) => it.kind === "file" && it.uploadStatus === KNOWLEDGE_UPLOAD_STATUS.DONE,
  ).length;

  const finishPackName = apiDatapackName ?? "";
  const handleWizardFinish = useCallback(() => {
    if (!finishPackName) return;
    if (!hasUploadedKnowledge) {
      toast({
        title: "Finished setup",
        description: `You're all set with ${finishPackName}.`,
        duration: 4000,
      });
    } else {
      startKnowledgeBaseImport(finishPackName, routeDatapackId);
      if (persistedKnowledgeStorageKey) {
        localStorage.removeItem(persistedKnowledgeStorageKey);
      }
    }
    navigate(datapackDetailPath(finishPackName, routeDatapackId));
  }, [finishPackName, hasUploadedKnowledge, navigate, persistedKnowledgeStorageKey, routeDatapackId]);

  const canProceedFromConfigure =
    Boolean(configureDraftName.trim()) &&
    !configureSubmitting &&
    !configureLoading &&
    Boolean(currentUser?.account_id);

  const headerDatapackName =
    configureDraftName.trim() ||
    apiDatapackName ||
    routeDatapackName ||
    (isCreateFlow ? "new datapack" : "");

  const breadcrumbItems = isCreateFlow
    ? [
      { label: "Datapacks", href: ROUTES.datapacks },
      { label: "New datapack", href: ROUTES.datapacks },
      { label: "configure" },
    ]
    : [
      { label: "Datapacks", href: ROUTES.datapacks },
      { label: routeDatapackName ?? "", href: datapackDetailPath(routeDatapackName ?? "", routeDatapackId) },
      { label: "configure" },
    ];

  return (
    <div className="flex h-screen flex-1 flex-col overflow-y-auto bg-background text-foreground">
      <AppWorkspaceHeader />

      <main className="mx-auto flex w-full max-w-6xl flex-1 flex-col space-y-8 p-8 pb-32">
        <div className="mx-auto w-full max-w-5xl space-y-8">
          <DatapackBreadcrumbs items={breadcrumbItems} />

          {/* When agent/MCP detection is wired, pass loading | connected instead of idle. */}
          <ConfigureWizardPageHeader
            datapackDisplayName={headerDatapackName}
            stepId={stepId}
            connectionStatus="idle"
            createFlowDescription={
              descriptionFromState && isCreateFlow && !apiDatapackName ? descriptionFromState : null
            }
          />

          <div className="w-full min-w-0 rounded-xl border border-border bg-muted/20 py-4 sm:py-5">
            <div className="w-full min-w-0 px-2 sm:px-3">
              <DatapackConfigureWizardStepper currentIndex={currentIndex} />
            </div>
          </div>

          {stepId === WIZARD_STEP_ID.configure ? (
            <ConfigureWizardConfigureStep
              isCreateFlow={isCreateFlow}
              name={configureDraftName}
              description={configureDraftDescription}
              onNameChange={setConfigureDraftName}
              onDescriptionChange={setConfigureDraftDescription}
              loading={configureLoading}
              submitting={configureSubmitting}
              error={configureError}
            />
          ) : null}
          {stepId === WIZARD_STEP_ID.install ? (
            <ConfigureWizardInstallStep
              mcpServerUrl={installProvision?.mcp_server_url ?? null}
              provisionLoading={installProvisionLoading}
            />
          ) : null}
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
          {stepId === WIZARD_STEP_ID.import && finishPackName ? (
            <ConfigureWizardStatusStep knowledgeItems={knowledgeItems} />
          ) : null}

          <ConfigureWizardUnifiedFooter
            stepId={stepId}
            datapackName={headerDatapackName}
            isCreateFlow={isCreateFlow}
            backButtonLabel={backButtonLabel}
            onBack={goBack}
            showBack={stepId !== WIZARD_STEP_ID.install}
            configureSubmitting={configureSubmitting}
            canProceedFromConfigure={canProceedFromConfigure}
            onConfigureSubmit={() => void handleConfigureStepNext()}
            syncKnowledgeCount={knowledgeItems.length}
            syncConversationsCount={0}
            syncSkillsCount={0}
            canImport={syncAllKnowledgeUploaded}
            onImport={() => void handleWizardNext()}
            onSkipForward={() => {
              if (stepId === WIZARD_STEP_ID.install) setStepId(WIZARD_STEP_ID.sync);
              else if (stepId === WIZARD_STEP_ID.sync) setStepId(WIZARD_STEP_ID.import);
            }}
            importKnowledgeCount={knowledgeDoneCount}
            importConversationsCount={0}
            importSkillsCount={0}
            hasUploadedKnowledge={hasUploadedKnowledge}
            onFinish={handleWizardFinish}
          />
        </div>
      </main>
    </div>
  );
}
