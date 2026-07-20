import { Link } from "react-router-dom";
import { ChevronLeft } from "lucide-react";
import { datapackDetailPath } from "@/app/routes/paths";
import { DatapackClustersPanel } from "@/features/datapacks/components/datapack-detail/DatapackClustersPanel";

/**
 * Full clusters view for a datapack (AMP): search clusters and expand one to
 * see its branch tree + provision compute. Reached via the "View all" footer on
 * the Clusters tile. Gated upstream by SHOW_PG_AMP.
 */
export default function DatapackClustersPage({ datapackId }: { datapackId: string }) {
  return (
    <main className="mx-auto w-full max-w-5xl px-4 py-6 sm:px-6 sm:py-8">
      <Link
        to={datapackDetailPath(datapackId)}
        className="mb-4 inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
      >
        <ChevronLeft className="h-4 w-4" aria-hidden />
        Back to datapack
      </Link>
      <h1 className="mb-4 text-2xl font-bold tracking-tight text-foreground">Clusters</h1>
      <DatapackClustersPanel datapackId={datapackId} />
    </main>
  );
}
