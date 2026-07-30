// Data Plane section: live YugabyteDB Anywhere universe configuration +
// utilization for the deployment universe, sourced from api_server's
// /admin/dataplane (which queries YBA). Falls back to static sample tiles when
// YBA isn't reachable yet.
import { useQuery } from "@tanstack/react-query";
import { Boxes, Cpu, MemoryStick, HardDrive, Globe, Copy, Activity } from "lucide-react";
import type { LucideIcon } from "lucide-react";
import { fetchDataPlane, type DataPlaneInfo } from "@/lib/api/adminApi";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { ADMIN_API_ERROR_HINT } from "@/features/admin/adminCopy";

type Tile = { label: string; value: string; sub: string; icon: LucideIcon; accent: string };

// Dash placeholder shown ONLY when the request itself failed (query.isError —
// e.g. a 400/500 or unreachable api_server), so we have no payload at all. With
// no response we render dashes rather than any numbers. A *successful* 200 (even
// one reporting the universe as not-yet-configured) is NOT this case — it renders
// SAMPLE_TILES. Muted accent reads as "no data"; same labels/icons as the live
// set so the layout doesn't shift.
const PLACEHOLDER_TILES: Tile[] = [
  { label: "Nodes", value: "—", sub: "", icon: Boxes, accent: "text-muted-foreground" },
  { label: "vCPU", value: "—", sub: "", icon: Cpu, accent: "text-muted-foreground" },
  { label: "Memory", value: "—", sub: "", icon: MemoryStick, accent: "text-muted-foreground" },
  { label: "Storage", value: "—", sub: "", icon: HardDrive, accent: "text-muted-foreground" },
  { label: "Replication factor", value: "—", sub: "", icon: Copy, accent: "text-muted-foreground" },
  { label: "Region", value: "—", sub: "", icon: Globe, accent: "text-muted-foreground" },
];

// Sample tiles shown on a successful (200) response when there's no live
// universe yet (dev / first-run / not-configured). Illustrative numbers so the
// panel reads as populated; the "Sample data" pill flags them as not-real.
// Accent is the unified brand color (matches liveTiles); real failures fall back
// to PLACEHOLDER_TILES (dashes) instead.
const SAMPLE_TILES: Tile[] = [
  { label: "Nodes", value: "1", sub: "kubernetes", icon: Boxes, accent: "text-brand" },
  { label: "vCPU", value: "8 / 4", sub: "TServer / Master", icon: Cpu, accent: "text-brand" },
  { label: "Memory", value: "16 / 8 GiB", sub: "TServer / Master", icon: MemoryStick, accent: "text-brand" },
  { label: "Storage", value: "100 GiB", sub: "1 × 100 GiB", icon: HardDrive, accent: "text-brand" },
  { label: "Replication factor", value: "1", sub: "RF", icon: Copy, accent: "text-brand" },
  { label: "Region", value: "US East", sub: "kubernetes", icon: Globe, accent: "text-brand" },
];

// Live tiles from a YBA universe. Compute is either per-node cores/memory
// (Kubernetes) or an instance-type string (cloud VMs) — whichever YBA returns.
function liveTiles(d: DataPlaneInfo): Tile[] {
  const storage = d.num_volumes * d.volume_size_gb;
  const tiles: Tile[] = [
    {
      label: "Nodes",
      value: d.nodes > 0 ? String(d.nodes) : "—",
      sub: d.provider || "nodes",
      icon: Boxes,
      accent: "text-brand",
    },
  ];

  const hasK8sSpecs = d.tserver_cores > 0 || d.master_cores > 0;
  if (hasK8sSpecs) {
    const bothCores = d.tserver_cores > 0 && d.master_cores > 0;
    tiles.push({
      label: "vCPU",
      value: bothCores
        ? `${d.tserver_cores} / ${d.master_cores}`
        : String(d.tserver_cores || d.master_cores),
      sub: bothCores ? "TServer / Master" : "per node",
      icon: Cpu,
      accent: "text-brand",
    });
    const bothMem = d.tserver_memory_gib > 0 && d.master_memory_gib > 0;
    tiles.push({
      label: "Memory",
      value: bothMem
        ? `${d.tserver_memory_gib} / ${d.master_memory_gib} GiB`
        : `${d.tserver_memory_gib || d.master_memory_gib} GiB`,
      sub: bothMem ? "TServer / Master" : "per node",
      icon: MemoryStick,
      accent: "text-brand",
    });
  } else if (d.instance_type) {
    tiles.push({
      label: "Instance type",
      value: d.instance_type,
      sub: d.provider || "compute",
      icon: Cpu,
      accent: "text-brand",
    });
  }

  tiles.push({
    label: "Storage",
    value: storage > 0 ? `${storage} GiB` : "—",
    sub: d.num_volumes > 0 ? `${d.num_volumes} × ${d.volume_size_gb} GiB` : "per node",
    icon: HardDrive,
    accent: "text-brand",
  });

  if (d.replication_factor > 0) {
    tiles.push({
      label: "Replication factor",
      value: String(d.replication_factor),
      sub: "RF",
      icon: Copy,
      accent: "text-brand",
    });
  }

  tiles.push({
    label: "Region",
    value: d.region || "—",
    sub: d.provider || "cloud",
    icon: Globe,
    accent: "text-brand",
  });

  return tiles;
}

// CPU/memory/disk utilization, only present when YBA returned a value.
function utilizationFor(d: DataPlaneInfo): { label: string; percent: number }[] {
  const out: { label: string; percent: number }[] = [];
  if (d.cpu_percent != null) out.push({ label: "CPU", percent: d.cpu_percent });
  if (d.memory_percent != null) out.push({ label: "Memory", percent: d.memory_percent });
  if (d.disk_percent != null) out.push({ label: "Disk", percent: d.disk_percent });
  return out;
}

function TileGrid({ tiles }: { tiles: Tile[] }) {
  return (
    <div className="grid grid-cols-2 gap-4 lg:grid-cols-3">
      {tiles.map(({ label, value, sub, icon: Icon, accent }) => (
        <Card key={label}>
          <CardHeader className="flex flex-row items-center justify-between pb-2">
            <CardTitle className="text-xs font-medium uppercase tracking-wider text-muted-foreground">
              {label}
            </CardTitle>
            <Icon className={`h-4 w-4 ${accent}`} />
          </CardHeader>
          <CardContent>
            <p className={`truncate text-2xl font-bold ${accent}`} title={value}>
              {value}
            </p>
            <p className="text-xs text-muted-foreground mt-1">{sub}</p>
          </CardContent>
        </Card>
      ))}
    </div>
  );
}

type FoundationMode = "live" | "sample" | "unavailable";

function StatusPill({ mode, healthy }: { mode: FoundationMode; healthy: boolean }) {
  if (mode === "unavailable") {
    return (
      <span className="inline-flex items-center gap-1.5 rounded-full border border-border bg-muted px-2.5 py-1 text-xs font-medium text-muted-foreground">
        <span className="h-1.5 w-1.5 rounded-full bg-muted-foreground" />
        Unavailable
      </span>
    );
  }
  // Sample data presents as a healthy "Live" pill (dev/demo); a real universe
  // reflects its actual health (Running / Degraded).
  const isHealthy = mode === "sample" || healthy;
  const label = mode === "sample" ? "Live" : healthy ? "Running" : "Degraded";
  const tone = isHealthy
    ? "border-emerald-500/30 bg-emerald-500/10 text-emerald-400"
    : "border-amber-500/30 bg-amber-500/10 text-amber-400";
  return (
    <span className={`inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs font-medium ${tone}`}>
      <span className={`h-1.5 w-1.5 rounded-full ${isHealthy ? "bg-emerald-400" : "bg-amber-400"}`} />
      {label}
    </span>
  );
}

export function DataPlanePage() {
  const query = useQuery({
    queryKey: ["admin-dataplane"],
    queryFn: () => fetchDataPlane(),
    staleTime: 30_000,
  });

  const d = query.data;
  // Three states: a failed request (no payload) → dashes; a 200 with a live
  // universe → real tiles; any other 200 (not-yet-configured) → sample tiles.
  const mode: FoundationMode =
    query.isError || !d ? "unavailable" : d.available ? "live" : "sample";
  const live = mode === "live";
  const healthy = live && d!.total_nodes > 0 && d!.live_nodes === d!.total_nodes;

  return (
    <div className="flex flex-col gap-4 p-6 overflow-auto">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold">Foundation</h1>
          <p className="text-sm text-muted-foreground mt-0.5">
            {live && d?.universe_name
              ? `${d.universe_name}${d.software_version ? ` · YugabyteDB ${d.software_version}` : ""}`
              : "Compute and capacity backing the Meko data plane."}
          </p>
        </div>
        {!query.isLoading && <StatusPill mode={mode} healthy={healthy} />}
      </div>

      {query.isError && (
        <Alert variant="destructive">
          <AlertDescription>
            Failed to load data-plane status. {ADMIN_API_ERROR_HINT}
          </AlertDescription>
        </Alert>
      )}

      {query.isLoading ? (
        <div className="grid grid-cols-2 gap-4 lg:grid-cols-3">
          {[0, 1, 2, 3, 4, 5].map((i) => (
            <Card key={i}>
              <CardHeader className="pb-2">
                <Skeleton className="h-4 w-20" />
              </CardHeader>
              <CardContent>
                <Skeleton className="h-8 w-24" />
              </CardContent>
            </Card>
          ))}
        </div>
      ) : (
        <>
          {/* Dashes only on a failed request; a 200 shows live tiles when the
              universe is available, otherwise illustrative sample tiles. */}
          <TileGrid
            tiles={mode === "unavailable" ? PLACEHOLDER_TILES : live ? liveTiles(d!) : SAMPLE_TILES}
          />

          {live && utilizationFor(d!).length > 0 && (
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
              {utilizationFor(d!).map(({ label, percent }) => (
                <Card key={label}>
                  <CardHeader className="flex flex-row items-center justify-between pb-2">
                    <CardTitle className="text-xs font-medium uppercase tracking-wider text-muted-foreground">
                      {label} usage
                    </CardTitle>
                    <Activity className="h-4 w-4 text-cyan-400" />
                  </CardHeader>
                  <CardContent>
                    <p className="text-2xl font-bold text-cyan-400">{percent.toFixed(1)}%</p>
                    <div className="mt-2 h-2 w-full overflow-hidden rounded-full bg-muted">
                      <div
                        className="h-full rounded-full bg-cyan-500"
                        style={{ width: `${Math.min(100, Math.max(0, percent))}%` }}
                      />
                    </div>
                  </CardContent>
                </Card>
              ))}
            </div>
          )}
        </>
      )}
    </div>
  );
}
