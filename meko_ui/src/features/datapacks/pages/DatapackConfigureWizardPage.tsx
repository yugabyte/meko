/**
 * Configure wizard:
 * - `/datapacks/new/configure` — create flow (Configure creates datapack, then Install → Add Knowledge → Finish).
 * - `/datapacks/:name/configure` — existing datapack (same steps; Configure can be skipped).
 */
import { useCallback, useEffect, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { dispatchAppEvent } from "@/lib/browserEvents";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { DatapackConfigureWizardStepper } from "@/features/datapacks/components/configure-wizard/DatapackConfigureWizardStepper";
import { ConfigureWizardConfigureStep } from "@/features/datapacks/components/configure-wizard/ConfigureWizardConfigureStep";
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
import { datapackDetailPath, ROUTES } from "@/app/routes/paths";
import { markDatapackWizardConfigureDone } from "@/lib/datapackConfigureStatus";
import {
  confirmUpload,
  createDatapack,
  DATAPACKS_LIST_UPDATED_EVENT,
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
  filterFilesForKnowledgeUploadBatch,
} from "@/lib/knowledgeFileUploadRules";
import { startKnowledgeBaseImport } from "@/lib/knowledgeBaseImportJob";
import { KNOWLEDGE_UPLOAD_STATUS } from "@/lib/knowledgeUploadStatus";
import { toast } from "@/hooks/use-toast";

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

type DatapackConfigureWizardPageProps = {
  currentUser: CurrentUser | null;
  /** `/datapacks/new/configure` */
  isCreateFlow?: boolean;
  /** `/datapacks/:name/configure` */
  datapackName?: string;
};

export default function DatapackConfigureWizardPage({
  currentUser,
  isCreateFlow = false,
  datapackName: routeDatapackName,
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
    void getDatapack(routeDatapackName)
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
    ...(includeUploadMeta ? { file: f, uploadStatus: KNOWLEDGE_UPLOAD_STATUS.PENDING } : {}),
  });

  const addFiles = (section: SyncSectionId, files: FileList | null) => {
    if (section !== "knowledge" || !files?.length) return;
    setKnowledgeItems((prev) => {
      const staging = prev.filter((it) => it.kind === "file" && it.uploadStatus !== KNOWLEDGE_UPLOAD_STATUS.DONE)
        .length;
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

  const handleUploadKnowledgeFiles = useCallback(async () => {
    const pack = apiDatapackName;
    if (!pack) return;
    setKnowledgeUploading(true);
    try {
      const pending = knowledgeItems.filter(
        (it) =>
          it.kind === "file" &&
          it.file &&
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
          setKnowledgeItems((prev) =>
            prev.map((x) =>
              x.id === item.id
                ? {
                  ...x,
                  uploadStatus: KNOWLEDGE_UPLOAD_STATUS.ERROR,
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
            });
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
          dispatchAppEvent(DATAPACKS_LIST_UPDATED_EVENT);
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
        await updateDatapack(routeName, {
          name: trimmed,
          description: configureDraftDescription,
        });
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

  const skipConfigureStep = useCallback(() => {
    setConfigureError(null);
    setStepId(WIZARD_STEP_ID.install);
  }, []);

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

  /** After create, do not return to Configure (datapack already exists); exit to overview instead. */
  const createFlowCommitted = isCreateFlow && Boolean(apiDatapackName);
  const backExitsCreateWizardToDatapacks =
    createFlowCommitted && stepId !== WIZARD_STEP_ID.configure;

  const goBack = () => {
    if (backExitsCreateWizardToDatapacks) {
      navigate(ROUTES.datapacks);
      return;
    }
    if (currentIndex === 0) {
      if (isCreateFlow) {
        navigate(ROUTES.datapacks);
      } else if (routeDatapackName) {
        navigate(datapackDetailPath(routeDatapackName));
      } else {
        navigate(ROUTES.datapacks);
      }
      return;
    }
    const prev = WIZARD_STEPS[currentIndex - 1];
    if (prev) setStepId(prev.id);
  };

  const backButtonLabel = backExitsCreateWizardToDatapacks
    ? "Back to datapacks"
    : currentIndex === 0
      ? isCreateFlow
        ? "Back to datapacks"
        : "Back"
      : "Back";

  const hasUploadedKnowledge = knowledgeItems.some((it) => it.uploadStatus === KNOWLEDGE_UPLOAD_STATUS.DONE);
  const knowledgeDoneCount = knowledgeItems.filter(
    (it) => it.kind === "file" && it.uploadStatus === KNOWLEDGE_UPLOAD_STATUS.DONE,
  ).length;

  const finishPackName = apiDatapackName ?? "";
  const handleWizardFinish = useCallback(() => {
    if (!finishPackName) return;
    markDatapackWizardConfigureDone(finishPackName);
    if (!hasUploadedKnowledge) {
      toast({
        title: "Finished setup",
        description: `You're all set with ${finishPackName}.`,
        duration: 4000,
      });
    } else {
      startKnowledgeBaseImport(finishPackName);
    }
    navigate(datapackDetailPath(finishPackName));
  }, [finishPackName, hasUploadedKnowledge, navigate]);

  const canGoNextFromSync = stepId !== WIZARD_STEP_ID.sync || syncAllKnowledgeUploaded;

  const canProceedFromConfigure =
    Boolean(configureDraftName.trim()) &&
    !configureSubmitting &&
    !configureLoading &&
    Boolean(currentUser?.account_id);

  const canWizardNext =
    stepId === WIZARD_STEP_ID.configure
      ? canProceedFromConfigure
      : stepId === WIZARD_STEP_ID.sync
        ? canGoNextFromSync
        : true;

  const displayTitleName = apiDatapackName ?? (isCreateFlow ? "new datapack" : routeDatapackName ?? "");

  const breadcrumbItems = isCreateFlow
    ? [
      { label: "Datapacks", href: ROUTES.datapacks },
      { label: "New datapack", href: ROUTES.datapacks },
      { label: "configure" },
    ]
    : [
      { label: "Datapacks", href: ROUTES.datapacks },
      { label: routeDatapackName ?? "", href: datapackDetailPath(routeDatapackName ?? "") },
      { label: "configure" },
    ];

  return (
    <div className="flex h-screen flex-1 flex-col overflow-y-auto bg-background text-foreground">
      <AppWorkspaceHeader />

      <main className="mx-auto flex w-full max-w-6xl flex-1 flex-col space-y-8 p-8 pb-32">
        <div className="mx-auto w-full max-w-5xl space-y-8">
          <DatapackBreadcrumbs items={breadcrumbItems} />

          <div className="space-y-2">
            <h1 className="text-2xl font-semibold tracking-tight sm:text-3xl">
              Configure <span className="text-foreground">{displayTitleName}</span>
            </h1>
            {descriptionFromState && isCreateFlow && !apiDatapackName ? (
              <p className="max-w-3xl text-sm leading-relaxed text-muted-foreground">{descriptionFromState}</p>
            ) : (
              <p className="max-w-3xl text-sm leading-relaxed text-muted-foreground">
                Finish setup: configure the datapack, install the client, add knowledge files, then review and
                finish.
              </p>
            )}
          </div>

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
              mcpApiKey={installProvision?.mcp_api_key ?? null}
              provisionLoading={installProvisionLoading}
              provisionError={installProvisionError}
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
            <ConfigureWizardStatusStep
              datapackName={finishPackName}
              knowledgeItems={knowledgeItems}
              knowledgeCount={knowledgeDoneCount}
              conversationsCount={0}
              skillsCount={0}
              hasUploadedKnowledge={hasUploadedKnowledge}
              onFinish={handleWizardFinish}
            />
          ) : null}

          <ConfigureWizardNavBar
            stepId={stepId}
            currentIndex={currentIndex}
            backButtonLabel={backButtonLabel}
            onBack={goBack}
            onNext={() => void handleWizardNext()}
            onSkipStep={
              stepId === WIZARD_STEP_ID.configure && !isCreateFlow
                ? skipConfigureStep
                : stepId === WIZARD_STEP_ID.install || stepId === WIZARD_STEP_ID.sync
                  ? () => {
                    if (stepId === WIZARD_STEP_ID.install) setStepId(WIZARD_STEP_ID.sync);
                    else if (stepId === WIZARD_STEP_ID.sync) setStepId(WIZARD_STEP_ID.import);
                  }
                  : undefined
            }
            canGoNext={canWizardNext}
            nextDisabledTooltip="Upload knowledge files successfully before continuing, or use Skip step."
          />
        </div>
      </main>
    </div>
  );
}
