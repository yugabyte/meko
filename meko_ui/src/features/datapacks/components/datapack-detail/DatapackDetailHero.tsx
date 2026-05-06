/**
 * Top-of-page identity for a single datapack: breadcrumbs, framed icon + title row (tags),
 * description, and ID / created / access on one muted line. Renders an `actions` slot on the right.
 */
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { Database, Sparkles } from "lucide-react";
import { ROUTES } from "@/app/routes/paths";
import { cn } from "@/lib/utils";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
import { formatDatapackShareGrantCamelCase } from "@/lib/datapackShareGrants";
import { DatapackShareGrant } from "@/lib/mekoApi";

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
  /** Hero actions (e.g. Connect, Share, settings menu). */
  actions: React.ReactNode;
};

function ActiveStatusPill() {
  return (
    <span className="inline-flex items-center gap-1.5 rounded-full border border-emerald-500/45 bg-emerald-950/45 px-2 py-0.5 text-[11px] font-semibold tracking-wide text-emerald-100/95">
      <span className="size-1.5 shrink-0 rounded-full bg-emerald-400 shadow-[0_0_6px_rgba(52,211,153,0.65)]" aria-hidden />
      active
    </span>
  );
}

function NewDatapackPill() {
  return (
    <span className="inline-flex items-center gap-1 rounded-full border border-blue-500/45 bg-blue-950/40 px-2 py-0.5 text-[11px] font-semibold tracking-wide text-blue-100/95">
      <Sparkles className="size-3 shrink-0 text-blue-300/95" aria-hidden />
      new
    </span>
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
  actions,
}: Props) {
  const [descExpanded, setDescExpanded] = useState(false);
  const [descriptionOverflows, setDescriptionOverflows] = useState(false);
  const descRef = useRef<HTMLParagraphElement>(null);
  const trimmedDesc = (description ?? "").trim();
  const showDescription = trimmedDesc.length > 0;

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
      <DatapackBreadcrumbs
        items={[
          { label: "Datapacks", href: ROUTES.datapacks },
          { label: datapackName },
        ]}
      />

      <div className="flex flex-col gap-4 lg:flex-row lg:items-center lg:justify-between lg:gap-6">
        <div className="flex min-w-0 flex-1 gap-4 sm:gap-5">
          <div
            className="flex h-14 w-14 shrink-0 items-center justify-center rounded-lg border border-blue-500/35 bg-muted/15 shadow-sm"
            aria-hidden
          >
            <Database className="h-7 w-7 text-blue-400" />
          </div>

          <div className="min-w-0 flex-1 space-y-1">
            <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1.5">
              <h1 className="min-w-0 max-w-full truncate font-mono text-xl font-semibold tracking-tight text-foreground sm:text-2xl">
                {datapackName}
              </h1>
              {SHOW_DATAPACK_ACTIVE_STATUS_TAG ? <ActiveStatusPill /> : null}
              {isNewDatapack ? <NewDatapackPill /> : null}
            </div>

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
                    className="text-xs font-medium text-blue-400 underline underline-offset-2 hover:text-blue-300"
                    onClick={() => setDescExpanded((v) => !v)}
                  >
                    {descExpanded ? "Show less" : "Show more"}
                  </button>
                ) : null}
              </div>
            ) : null}

            <p className="flex flex-wrap items-baseline gap-x-1.5 gap-y-1 pt-0.5 text-[11px] leading-relaxed text-muted-foreground sm:text-xs">
              <span className="inline-flex flex-wrap items-baseline gap-x-1">
                <span className="font-semibold text-foreground/90">ID</span>
                <code className="rounded bg-muted/45 px-1 py-px font-mono text-[11px] text-foreground/90 sm:text-xs">
                  {statusLoading ? "…" : displayId}
                </code>
              </span>
              <span className="text-muted-foreground/45 select-none" aria-hidden>
                ·
              </span>
              <span className="inline-flex flex-wrap items-baseline gap-x-1">
                <span className="font-semibold text-foreground/90">Created</span>
                <span className="text-muted-foreground">{statusLoading ? "…" : createdLabel}</span>
              </span>
              <span className="text-muted-foreground/45 select-none" aria-hidden>
                ·
              </span>
              <span className="inline-flex flex-wrap items-baseline gap-x-1">
                <span className="font-semibold text-foreground/90">Access</span>
                <span className="text-muted-foreground">{accessLabel}</span>
              </span>
            </p>
          </div>
        </div>

        <div className="flex shrink-0 flex-wrap items-center gap-2">{actions}</div>
      </div>
    </>
  );
}
