/**
 * Top-of-page identity for a single datapack: breadcrumbs, title, status badges,
 * marketing-style description, and ID / logical DB / created metadata. Renders
 * an `actions` slot (e.g. Connect and upload dialogs) on the right on large screens.
 */
import { Database } from "lucide-react";
import { ROUTES } from "@/app/routes/paths";
import { DatapackBreadcrumbs } from "@/features/datapacks/components/datapack-detail/DatapackBreadcrumbs";
type Props = {
  datapackName: string;
  displayId: string;
  createdLabel: string;
  statusLoading: boolean;
  /** Hero actions (e.g. Add Knowledge, Connect, Configure, Share). */
  actions: React.ReactNode;
};

export function DatapackDetailHero({
  datapackName,
  displayId,
  createdLabel,
  statusLoading,
  actions,
}: Props) {
  return (
    <>
      <DatapackBreadcrumbs
        items={[
          { label: "Datapacks", href: ROUTES.datapacks },
          { label: datapackName },
        ]}
      />

      <div className="flex flex-col gap-4 lg:flex-row lg:items-start lg:justify-between">
        <div className="space-y-3 min-w-0">
          <div className="flex flex-wrap items-center gap-3">
            <Database className="w-8 h-8 text-blue-400 shrink-0" />
            <h1 className="text-2xl sm:text-3xl font-semibold tracking-tight text-foreground truncate">
              {datapackName}
            </h1>
          </div>
          <p className="text-sm text-muted-foreground max-w-2xl leading-relaxed">
            A secure, scalable data store for agents. Mapped to a dedicated YugabyteDB YSQL logical database.
          </p>
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

        <div className="flex flex-wrap items-center gap-2 shrink-0">{actions}</div>
      </div>
    </>
  );
}
