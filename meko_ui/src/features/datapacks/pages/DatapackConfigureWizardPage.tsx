/**
 * Route `/datapacks/:name/configure`: post-create wizard (install → detect → sync → status).
 * Composes modular step components; keeps sync list state and navigation here so steps stay presentational.
 */
import { useCallback, useRef, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { DatapackConfigureWizardStepper } from "@/features/datapacks/components/configure-wizard/DatapackConfigureWizardStepper";
import { ConfigureWizardDetectStep } from "@/features/datapacks/components/configure-wizard/ConfigureWizardDetectStep";
import { ConfigureWizardFooterBar } from "@/features/datapacks/components/configure-wizard/ConfigureWizardFooterBar";
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

function syncSectionTitle(section: SyncSectionId): string {
  if (section === "knowledge") return "Knowledge";
  if (section === "conversations") return "Conversations";
  return "Skills";
}

type Props = {
  datapackName: string;
};

export default function DatapackConfigureWizardPage({ datapackName }: Props) {
  const navigate = useNavigate();
  const location = useLocation();
  // Passed from DatapackNewPage after create (optional); not persisted server-side today.
  const descriptionFromState = (location.state as { description?: string } | null)?.description;

  const [stepId, setStepId] = useState<WizardStepId>(WIZARD_STEP_ID.install);
  const currentIndex = wizardStepIndex(stepId);

  const [knowledgeItems, setKnowledgeItems] = useState<ListedSource[]>([]);
  const [conversationItems, setConversationItems] = useState<ListedSource[]>([]);
  const [skillItems, setSkillItems] = useState<ListedSource[]>([]);

  const knowledgeInputRef = useRef<HTMLInputElement>(null);
  const conversationInputRef = useRef<HTMLInputElement>(null);
  const skillsInputRef = useRef<HTMLInputElement>(null);

  const [urlDialogOpen, setUrlDialogOpen] = useState(false);
  const [urlTargetSection, setUrlTargetSection] = useState<SyncSectionId | null>(null);
  const [urlDraft, setUrlDraft] = useState("");

  const goDetail = () => navigate(datapackDetailPath(datapackName));

  const fileEntry = (f: File): ListedSource => ({
    id: `${f.name}-${f.size}-${globalThis.crypto?.randomUUID?.() ?? Math.random().toString(36).slice(2)}`,
    label: f.name,
    kind: "file",
  });

  const addFiles = (section: SyncSectionId, files: FileList | null) => {
    if (!files?.length) return;
    if (section === "knowledge") {
      setKnowledgeItems((prev) => [...prev, ...Array.from(files).map(fileEntry)]);
    } else if (section === "conversations") {
      setConversationItems((prev) => [...prev, ...Array.from(files).map(fileEntry)]);
    } else {
      // Skills tile only keeps Markdown filenames even if the picker allows broader types.
      const mdOnly = Array.from(files).filter((f) => f.name.toLowerCase().endsWith(".md"));
      setSkillItems((prev) => [...prev, ...mdOnly.map(fileEntry)]);
    }
  };

  const openUrlDialog = (section: SyncSectionId) => {
    setUrlTargetSection(section);
    setUrlDraft("");
    setUrlDialogOpen(true);
  };

  const closeUrlDialog = useCallback(() => {
    setUrlDialogOpen(false);
    setUrlTargetSection(null);
    setUrlDraft("");
  }, []);

  const commitUrl = () => {
    const section = urlTargetSection;
    const raw = urlDraft.trim();
    if (!section || !raw) return;
    const id = globalThis.crypto?.randomUUID?.() ?? String(Date.now());
    const item: ListedSource = { id, label: raw, kind: "url" };
    if (section === "knowledge") setKnowledgeItems((p) => [...p, item]);
    else if (section === "conversations") setConversationItems((p) => [...p, item]);
    else setSkillItems((p) => [...p, item]);
    closeUrlDialog();
  };

  const removeItem = (section: SyncSectionId, id: string) => {
    if (section === "knowledge") setKnowledgeItems((p) => p.filter((x) => x.id !== id));
    else if (section === "conversations") setConversationItems((p) => p.filter((x) => x.id !== id));
    else setSkillItems((p) => p.filter((x) => x.id !== id));
  };

  const goNext = () => {
    const next = WIZARD_STEPS[currentIndex + 1];
    if (next) setStepId(next.id);
  };

  const goBack = () => {
    const prev = WIZARD_STEPS[currentIndex - 1];
    if (prev) setStepId(prev.id);
    else navigate(ROUTES.datapacks);
  };

  // Footer: same summary strip as the mock once the user can attach sources or review final status.
  const showFooter = stepId === WIZARD_STEP_ID.sync;

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
                Finish setup: install the client, verify detection, choose what to sync, then review status.
              </p>
            )}
          </div>

          <div className="rounded-xl border border-border bg-muted/20 px-4 py-5 sm:px-6">
            <DatapackConfigureWizardStepper currentIndex={currentIndex} />
          </div>

          {stepId === WIZARD_STEP_ID.install ? <ConfigureWizardInstallStep /> : null}
          {stepId === WIZARD_STEP_ID.detect ? <ConfigureWizardDetectStep /> : null}
          {stepId === WIZARD_STEP_ID.sync ? (
            <ConfigureWizardSyncStep
              knowledgeItems={knowledgeItems}
              conversationItems={conversationItems}
              skillItems={skillItems}
              knowledgeInputRef={knowledgeInputRef}
              conversationInputRef={conversationInputRef}
              skillsInputRef={skillsInputRef}
              onPickFiles={addFiles}
              onAddUrl={openUrlDialog}
              onRemove={removeItem}
            />
          ) : null}
          {stepId === WIZARD_STEP_ID.status ? <ConfigureWizardStatusStep datapackName={datapackName} /> : null}

          {showFooter ? (
            <ConfigureWizardFooterBar
              datapackName={datapackName}
              knowledgeCount={knowledgeItems.length}
              conversationsCount={conversationItems.length}
              skillsCount={skillItems.length}
              onSkip={goDetail}
            />
          ) : null}

          <ConfigureWizardNavBar
            stepId={stepId}
            currentIndex={currentIndex}
            onBack={goBack}
            onNext={goNext}
            onOpenDatapack={goDetail}
          />

          <Dialog
            open={urlDialogOpen}
            onOpenChange={(open) => {
              if (!open) closeUrlDialog();
            }}
          >
            <DialogContent className="sm:max-w-md">
              <DialogHeader>
                <DialogTitle>
                  {urlTargetSection ? `Add URL — ${syncSectionTitle(urlTargetSection)}` : "Add URL"}
                </DialogTitle>
                <DialogDescription>
                  Enter a link to attach to this datapack section. You can use a full URL or any reference string you
                  want to track.
                </DialogDescription>
              </DialogHeader>
              <div className="space-y-2 py-1">
                <label htmlFor="configure-wizard-url" className="text-sm font-medium text-foreground">
                  URL or reference
                </label>
                <Input
                  id="configure-wizard-url"
                  type="text"
                  autoComplete="url"
                  autoFocus
                  placeholder="https://example.com/doc"
                  value={urlDraft}
                  onChange={(e) => setUrlDraft(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" && urlDraft.trim()) {
                      e.preventDefault();
                      commitUrl();
                    }
                  }}
                />
              </div>
              <DialogFooter className="gap-2 sm:gap-0">
                <Button type="button" variant="outline" onClick={closeUrlDialog}>
                  Cancel
                </Button>
                <Button type="button" onClick={commitUrl} disabled={!urlDraft.trim()}>
                  Add
                </Button>
              </DialogFooter>
            </DialogContent>
          </Dialog>
        </div>
      </main>
    </div>
  );
}
