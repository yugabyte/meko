/**
 * Top-of-page identity for a single datapack: breadcrumbs, title, optional description (with expand),
 * and ID / created metadata. Renders an `actions` slot on the right on large screens.
 */
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { Database } from "lucide-react";
import { ROUTES } from "@/app/routes/paths";
import { cn } from "@/lib/utils";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";

type Props = {
  datapackName: string;
  /** Shown below the title; empty or missing hides the block. */
  description?: string | null;
  displayId: string;
  createdLabel: string;
  statusLoading: boolean;
  /** Hero actions (e.g. Add Knowledge, Connect, Configure, Share). */
  actions: React.ReactNode;
};

export function DatapackDetailHero({
  datapackName,
  description,
  displayId,
  createdLabel,
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

  return (
    <>
      <DatapackBreadcrumbs
        items={[
          { label: "Datapacks", href: ROUTES.datapacks },
          { label: datapackName },
        ]}
      />

      <div className="flex flex-col gap-4 lg:flex-row lg:items-start lg:justify-between">
        {/* flex-1 + min-w-0: use remaining row width so description isn’t sized to title max-content only */}
        <div className="w-full min-w-0 flex-1 space-y-3">
          <div className="flex min-w-0 flex-wrap items-center gap-3">
            <Database className="h-8 w-8 shrink-0 text-blue-400" />
            <h1 className="truncate text-2xl font-semibold tracking-tight text-foreground sm:text-3xl">
              {datapackName}
            </h1>
          </div>
          {showDescription ? (
            <div className="w-full min-w-0 space-y-1">
              <p
                ref={descRef}
                className={cn(
                  "w-full min-w-0 text-sm leading-relaxed text-muted-foreground whitespace-pre-wrap break-words",
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
          <div className="flex flex-wrap gap-x-6 gap-y-2 text-xs text-muted-foreground">
            <div>
              <span className="font-medium text-foreground/80">ID </span>
              <code className="rounded bg-muted/50 px-1.5 py-0.5 text-foreground/90">
                {statusLoading ? "…" : displayId}
              </code>
            </div>
            <div>
              <span className="font-medium text-foreground/80">Created </span>
              <span>{statusLoading ? "…" : createdLabel}</span>
            </div>
          </div>
        </div>

        <div className="flex shrink-0 flex-wrap items-center gap-2">{actions}</div>
      </div>
    </>
  );
}
