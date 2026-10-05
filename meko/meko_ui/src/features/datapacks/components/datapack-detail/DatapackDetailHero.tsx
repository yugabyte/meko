/**
 * Top-of-page identity for a single datapack: breadcrumbs, framed icon + title row (tags),
 * description, and ID / created / access on one muted line. Renders an `actions` slot on the right.
 */
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { Check, Copy, Database, FolderTree, Sparkles } from "lucide-react";
import { projectDetailPath, ROUTES } from "@/app/routes/paths";
import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import { copyToClipboard } from "@/lib/clipboard";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { formatDatapackShareGrantCamelCase } from "@/lib/datapackShareGrants";
import { DatapackShareGrant } from "@/lib/api/mekoApi";
import type { DatapackPath } from "@/lib/api/projectsApi";
import { PROJECTS_TITLE, UNFILED_LABEL } from "@/features/projects/projectsCopy";

/**
 * When true, shows the green “active” status pill next to the title.
 * Replace with a real condition (e.g. from API) when product defines “active”; UI is ready behind this gate.
 */
const SHOW_DATAPACK_ACTIVE_STATUS_TAG = false;

type Props = {
  datapackName: string;
  datapackGrant: DatapackShareGrant | null;
  /** Shown below the title; empty or missing hides the block. */
  description?: string | null;
  displayId: string;
  createdLabel: string;
  /** Shown when `created_at` is within the last 24 hours. */
  isNewDatapack?: boolean;
  statusLoading: boolean;
  /** Who owns a datapack shared WITH you. Omitted on your own, where an owner
   *  line would only ever read as your own name. */
  ownerName?: string | null;
  /** Off with the projects feature disabled, which is what keeps the whole
   *  Project entry out rather than showing it permanently unfiled. */
  showProject?: boolean;
  /** Where this datapack is filed. Undefined means unfiled. */
  projectPath?: DatapackPath;
  /** Hero actions (e.g. Connect, Share, settings menu). */
  actions: React.ReactNode;
};

/** The separator between metadata entries. Its own component because the line
 *  now carries up to five of them and an inline copy each time is noise. */
function MetaDot() {
  return (
    <span className="text-muted-foreground/45 select-none" aria-hidden>
      ·
    </span>
  );
}

/** One `Label value` pair on the metadata line. */
function MetaItem({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <span className="inline-flex flex-wrap items-baseline gap-x-1">
      <span className="font-semibold text-foreground/90">{label}</span>
      {children}
    </span>
  );
}

function ActiveStatusPill() {
  return (
    <span className="inline-flex items-center gap-1.5 rounded-full border border-emerald-500/45 bg-emerald-500/10 dark:bg-emerald-950/45 px-2 py-0.5 text-[11px] font-semibold tracking-wide text-emerald-700 dark:text-emerald-100/95">
      <span className="size-1.5 shrink-0 rounded-full bg-emerald-400 shadow-[0_0_6px_rgba(52,211,153,0.65)]" aria-hidden />
      active
    </span>
  );
}

function NewDatapackPill() {
  return (
    <Badge variant="brand" className="gap-1 rounded-full px-2 py-0.5 text-[11px] tracking-wide">
      <Sparkles className="size-3 shrink-0" aria-hidden />
      new
    </Badge>
  );
}

export function DatapackDetailHero({
  datapackName,
  datapackGrant,
  description,
  displayId,
  createdLabel,
  isNewDatapack = false,
  statusLoading,
  ownerName,
  showProject = false,
  projectPath,
  actions,
}: Props) {
  const [descExpanded, setDescExpanded] = useState(false);
  const [descriptionOverflows, setDescriptionOverflows] = useState(false);
  const [idCopied, setIdCopied] = useState(false);
  const descRef = useRef<HTMLParagraphElement>(null);
  const trimmedDesc = (description ?? "").trim();
  const showDescription = trimmedDesc.length > 0;
  const canCopyId = !statusLoading && Boolean(displayId) && displayId !== "—";

  const handleCopyId = async () => {
    if (!canCopyId) return;
    const ok = await copyToClipboard(displayId);
    if (!ok) return;
    setIdCopied(true);
    window.setTimeout(() => setIdCopied(false), 1500);
  };

  useEffect(() => {
    setDescExpanded(false);
  }, [trimmedDesc]);

  useLayoutEffect(() => {
    const el = descRef.current;
    if (!el || !showDescription) {
      setDescriptionOverflows(false);
      return;
    }
    if (descExpanded) {
      return;
    }
    const measure = () => {
      setDescriptionOverflows(el.scrollHeight > el.clientHeight + 1);
    };
    measure();
    const ro = new ResizeObserver(() => measure());
    ro.observe(el);
    return () => ro.disconnect();
  }, [trimmedDesc, descExpanded, showDescription]);

  const accessLabel = datapackGrant != null ? formatDatapackShareGrantCamelCase(datapackGrant) : "-";

  return (
    <>
      {/* A filed datapack is reached from its project far more often than from
          the flat list, so the trail retraces that route and the way back is
          one click. Unfiled, there is no project to return to. */}
      <DatapackBreadcrumbs
        items={
          showProject && projectPath
            ? [
                { label: PROJECTS_TITLE, href: ROUTES.projects },
                {
                  label: projectPath.project_name,
                  href: projectDetailPath(projectPath.project_id),
                },
                { label: datapackName },
              ]
            : [
                { label: "Datapacks", href: ROUTES.datapacks },
                { label: datapackName },
              ]
        }
      />

      <div className="flex flex-col gap-4 lg:flex-row lg:items-center lg:justify-between lg:gap-6">
        <div className="flex min-w-0 flex-1 gap-4 sm:gap-5">
          <div
            className="flex h-14 w-14 shrink-0 items-center justify-center rounded-lg border border-brand/35 bg-muted/15 shadow-sm"
            aria-hidden
          >
            <Database className="h-7 w-7 text-brand" />
          </div>

          <div className="min-w-0 flex-1 space-y-1">
            <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1.5">
              <h1 className="min-w-0 max-w-full truncate font-mono text-xl font-semibold tracking-tight text-foreground sm:text-2xl">
                {datapackName}
              </h1>
              {SHOW_DATAPACK_ACTIVE_STATUS_TAG ? <ActiveStatusPill /> : null}
              {isNewDatapack ? <NewDatapackPill /> : null}
            </div>

            {/* Where this datapack lives, directly under its name. This used
                to sit in the ID / Created / Access line and got lost among
                three other facts; a project is not trivia about the
                datapack, it is its address. */}
            {showProject ? (
              projectPath ? (
                <Link
                  to={projectDetailPath(projectPath.project_id)}
                  title={projectPath.path}
                  className={cn(
                    "inline-flex max-w-full items-center gap-1.5 rounded-full border px-2.5 py-1",
                    "border-brand/25 bg-brand/10 text-xs font-medium text-brand",
                    "transition-colors hover:border-brand/45 hover:bg-brand/15",
                    "focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-brand",
                  )}
                >
                  <FolderTree className="h-3.5 w-3.5 shrink-0" aria-hidden />
                  <span className="truncate font-mono">{projectPath.path}</span>
                </Link>
              ) : (
                <span
                  className={cn(
                    "inline-flex items-center gap-1.5 rounded-full border border-dashed px-2.5 py-1",
                    "border-border text-xs text-muted-foreground",
                  )}
                >
                  <FolderTree className="h-3.5 w-3.5 shrink-0" aria-hidden />
                  {UNFILED_LABEL}
                </span>
              )
            ) : null}

            {showDescription ? (
              <div className="w-full min-w-0 space-y-1">
                <p
                  ref={descRef}
                  className={cn(
                    "w-full min-w-0 text-sm leading-snug text-muted-foreground whitespace-pre-wrap break-words",
                    !descExpanded && "line-clamp-2",
                  )}
                >
                  {trimmedDesc}
                </p>
                {descriptionOverflows || descExpanded ? (
                  <button
                    type="button"
                    className="text-xs font-medium text-brand underline underline-offset-2 hover:text-brand/80"
                    onClick={() => setDescExpanded((v) => !v)}
                  >
                    {descExpanded ? "Show less" : "Show more"}
                  </button>
                ) : null}
              </div>
            ) : null}

            <p className="flex flex-wrap items-baseline gap-x-1.5 gap-y-1 pt-0.5 text-[11px] leading-relaxed text-muted-foreground sm:text-xs">
              <span className="inline-flex flex-wrap items-center gap-x-1">
                <span className="font-semibold text-foreground/90">ID</span>
                <code className="rounded bg-muted/45 px-1 py-px font-mono text-[11px] text-foreground/90 sm:text-xs">
                  {statusLoading ? "…" : displayId}
                </code>
                {canCopyId ? (
                  <button
                    type="button"
                    onClick={() => void handleCopyId()}
                    className="inline-flex h-5 w-5 items-center justify-center rounded text-muted-foreground transition-colors hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-blue-500"
                    aria-label="Copy datapack ID"
                    title={idCopied ? "Copied" : "Copy ID"}
                  >
                    {idCopied ? (
                      <Check className="h-3.5 w-3.5 text-green-500" />
                    ) : (
                      <Copy className="h-3.5 w-3.5" />
                    )}
                  </button>
                ) : null}
              </span>
              <MetaDot />
              <MetaItem label="Created">
                <span className="text-muted-foreground">{statusLoading ? "…" : createdLabel}</span>
              </MetaItem>
              <MetaDot />
              <MetaItem label="Access">
                <span className="text-muted-foreground">{accessLabel}</span>
              </MetaItem>

              {/* Directly after Access, because on a shared datapack the two
                  answer one question together: what may I do here, and whose
                  is it. */}
              {ownerName ? (
                <>
                  <MetaDot />
                  <MetaItem label="Owner">
                    <span className="text-muted-foreground">{ownerName}</span>
                  </MetaItem>
                </>
              ) : null}

            </p>
          </div>
        </div>

        <div className="flex shrink-0 flex-wrap items-center gap-2">{actions}</div>
      </div>
    </>
  );
}
