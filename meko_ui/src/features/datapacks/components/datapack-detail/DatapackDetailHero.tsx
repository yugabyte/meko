/**
 * Top-of-page identity for a single datapack: breadcrumbs, title, status badges,
 * marketing-style description, and ID / logical DB / created metadata. Renders
 * an `actions` slot (e.g. Connect and upload dialogs) on the right on large screens.
 */
import { Link } from "react-router-dom";
import { ChevronRight, Database } from "lucide-react";
import { ROUTES } from "@/app/routes/paths";
type Props = {
  datapackName: string;
  displayId: string;
  createdLabel: string;
  statusLoading: boolean;
  /** Connect + upload controls (typically dialog triggers). */
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
      <nav className="flex items-center gap-1.5 text-sm text-muted-foreground">
        <Link to={ROUTES.datapacks} className="hover:text-foreground transition-colors">
          Datapacks
        </Link>
        <ChevronRight className="h-4 w-4 shrink-0 opacity-60" aria-hidden />
        <span className="text-foreground font-medium truncate">{datapackName}</span>
      </nav>

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
