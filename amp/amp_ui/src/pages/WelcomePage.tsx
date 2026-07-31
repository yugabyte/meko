// Home / landing page. Three stacked sections:
//   1. Hero — the platform pitch + an (illustrative) Architect prompt box.
//   2. Platform model cascade — Clusters → Shared pools → Foundations, the
//      same three tiers as the sidebar legend. The two counts (clusters,
//      shared pools) are live from the controller, scoped to the current user;
//      the workload/foundation chips are illustrative (no per-tenant API).
//   3. The agents — Voyager / Perf Advisor / Nexus cards (Architect lives in
//      the hero instead). Each opens its right-hand side panel.

import { useMemo } from "react";
import { useNavigate } from "react-router-dom";
import { ArrowRight, ArrowUpRight, Sparkles } from "lucide-react";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { AGENT_PANELS } from "@/components/layout/navConfig";
import { useAgentPanel } from "@/components/layout/AgentPanel";
import { useCurrentUser, useFleet, useStoragePools } from "@/lib/api/queries";

const ARCHITECT_SUGGESTIONS = [
  "24 tenant DBs, multi-region sync",
  "Timeseries store for metrics",
  "Migrate Oracle → HexaBridge",
];

// Illustrative chips — the platform's supported workloads / topologies. These
// are platform capabilities, not per-tenant data, so they're static.
const WORKLOAD_CHIPS = [
  { label: "PostgreSQL", className: "text-brand border-brand/30" },
];

export default function WelcomePage() {
  const navigate = useNavigate();
  const currentUser = useCurrentUser();
  const pools = useStoragePools();

  const myPools = useMemo(() => {
    if (!pools.data || !currentUser.data) return [];
    return pools.data.filter((p) => p.owner_user_id === currentUser.data.id);
  }, [pools.data, currentUser.data]);

  const fleet = useFleet(myPools);

  const loading = currentUser.isLoading || pools.isLoading || fleet.isLoading;
  const dbCount = loading ? "…" : String(fleet.rows.length);
  const poolCount = loading ? "…" : String(myPools.length);

  return (
    <div className="mx-auto max-w-6xl space-y-12 px-6 py-10">
      {/* 1. Hero */}
      <section className="overflow-hidden rounded-2xl border border-border bg-card/40 p-8 md:p-10">
        <span className="inline-flex items-center gap-2 rounded-full border border-teal-500/40 px-3 py-1 text-xs font-semibold text-teal-600 dark:text-teal-400">
          <span className="h-1.5 w-1.5 rounded-full bg-teal-400" />
          3 agents online · MCP endpoint active · 5 foundations
        </span>

        <h1 className="mt-6 max-w-3xl text-4xl font-extrabold leading-tight tracking-tight text-foreground md:text-5xl">
          A multitenant <span className="text-brand">PostgreSQL platform</span>{" "}
          your agents can run.
        </h1>

        <p className="mt-4 max-w-2xl text-base leading-relaxed text-muted-foreground">
          Fully Postgres-compatible, distributed and resilient. Pick a storage{" "}
          <span className="font-medium text-blue-500 dark:text-blue-400">
            foundation
          </span>
          , carve out a{" "}
          <span className="font-medium text-teal-600 dark:text-teal-400">
            shared pool
          </span>{" "}
          for your team, then let the agents spin up, migrate and tune a whole
          fleet of <span className="font-medium text-brand">clusters</span>.
        </p>

        {/* Illustrative Architect prompt box */}
        <div className="mt-8 max-w-2xl rounded-xl border border-border bg-background/60 p-4">
          <div className="flex items-center gap-2 text-sm font-semibold text-brand">
            <Sparkles className="h-4 w-4" />
            Architect agent
          </div>
          <p className="mt-3 text-sm text-muted-foreground">
            e.g. In the Payments pool, create 24 tenant clusters multi-region
            sync with pgvector enabled…
          </p>
          <div className="mt-4 flex flex-wrap items-center gap-2">
            {ARCHITECT_SUGGESTIONS.map((s) => (
              <span
                key={s}
                className="rounded-md border border-border bg-card px-2.5 py-1 text-xs text-muted-foreground"
              >
                {s}
              </span>
            ))}
            <Button
              onClick={() => navigate("/agents/architect")}
              variant="brand"
              className="ml-auto"
            >
              Design with Architect
              <ArrowRight className="h-4 w-4" />
            </Button>
          </div>
        </div>
      </section>

      {/* 2. Platform model cascade */}
      <section className="space-y-2">
        <CascadeRow
          dotClass="bg-brand"
          title="Clusters"
          sublabel="Workloads"
          meta="many per pool"
          count={dbCount}
          countUnit="dbs"
        >
          {WORKLOAD_CHIPS.map((w) => (
            <Chip key={w.label} className={w.className}>
              {w.label}
            </Chip>
          ))}
        </CascadeRow>

        <Connector label="grouped & governed by" />

        <CascadeRow
          dotClass="bg-teal-400"
          title="Shared pools"
          sublabel="Team / BU · tenancy"
          meta="many per foundation"
          count={poolCount}
          countUnit="pools"
        >
          {myPools.length ? (
            myPools.slice(0, 5).map((p) => (
              <Chip
                key={p.id}
                className="border-teal-500/30 text-teal-600 dark:text-teal-400"
              >
                {p.name}
              </Chip>
            ))
          ) : (
            <span className="text-xs text-muted-foreground">
              {loading ? "Loading pools…" : "No pools yet"}
            </span>
          )}
        </CascadeRow>

        <Connector label="deployed on" />

        <CascadeRow
          dotClass="bg-blue-500"
          title="Foundations"
          sublabel="Storage + topology"
          rightText="the substrate pools deploy on"
        >
          {null}
        </CascadeRow>
      </section>

      {/* 3. The agents */}
      <section>
        <div className="mb-5 flex flex-wrap items-center gap-3">
          <span className="text-xs font-semibold uppercase tracking-[0.14em] text-brand">
            The agents
          </span>
          <span className="text-sm text-muted-foreground">
            Every action also runs over{" "}
            <span className="font-medium text-foreground">MCP</span> or the{" "}
            <span className="font-medium text-foreground">REST API</span> — not
            just the UI
          </span>
        </div>

        <div className="grid grid-cols-1 gap-4 md:grid-cols-3">
          {Object.values(AGENT_PANELS).map((agent) => (
            <AgentCard key={agent.id} agent={agent} />
          ))}
        </div>
      </section>
    </div>
  );
}

function Chip({
  children,
  className,
}: {
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <span
      className={cn(
        "rounded-md border px-2.5 py-0.5 text-xs font-medium",
        className,
      )}
    >
      {children}
    </span>
  );
}

function Connector({ label }: { label: string }) {
  return (
    <div className="flex items-center gap-3 px-4">
      <span className="h-4 w-px bg-border" />
      <span className="font-mono text-[11px] text-muted-foreground">
        {label}
      </span>
    </div>
  );
}

function CascadeRow({
  dotClass,
  title,
  sublabel,
  children,
  meta,
  count,
  countUnit,
  rightText,
}: {
  dotClass: string;
  title: string;
  sublabel: string;
  children: React.ReactNode;
  meta?: string;
  count?: string;
  countUnit?: string;
  rightText?: string;
}) {
  return (
    <div className="flex flex-wrap items-center gap-4 rounded-xl border border-border bg-card/40 p-4">
      <div className="flex min-w-[170px] items-center gap-3">
        <span className={cn("h-2.5 w-2.5 shrink-0 rounded-full", dotClass)} />
        <div>
          <div className="font-semibold text-foreground">{title}</div>
          <div className="text-[10px] font-medium uppercase tracking-[0.12em] text-muted-foreground">
            {sublabel}
          </div>
        </div>
      </div>

      <div className="flex flex-1 flex-wrap items-center gap-2">{children}</div>

      {meta && (
        <div className="hidden font-mono text-xs text-muted-foreground lg:block">
          {meta}
        </div>
      )}

      <div className="ml-auto text-right">
        {count !== undefined ? (
          <div className="text-lg font-bold text-foreground">
            {count}{" "}
            <span className="text-sm font-normal text-muted-foreground">
              {countUnit}
            </span>
          </div>
        ) : (
          <div className="max-w-[180px] text-xs text-muted-foreground">
            {rightText}
          </div>
        )}
      </div>
    </div>
  );
}

function AgentCard({
  agent,
}: {
  agent: (typeof AGENT_PANELS)[keyof typeof AGENT_PANELS];
}) {
  const { open } = useAgentPanel();
  const Icon = agent.icon;
  return (
    <button
      type="button"
      onClick={() => open(agent.id)}
      className="group relative rounded-xl border border-border bg-card/40 p-5 text-left transition-colors hover:border-brand/40"
    >
      <div className="absolute right-4 top-4 flex items-center gap-2">
        {agent.isNew && (
          <span className="rounded-md bg-emerald-500/15 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-emerald-600 dark:text-emerald-400">
            New
          </span>
        )}
        <ArrowUpRight className="h-4 w-4 text-muted-foreground opacity-0 transition-opacity group-hover:opacity-100" />
      </div>

      <span
        className={cn(
          "flex h-11 w-11 items-center justify-center rounded-xl",
          agent.tileClass,
        )}
      >
        <Icon className="h-5 w-5" />
      </span>

      <h3 className="mt-4 text-lg font-semibold text-foreground">
        {agent.name}
      </h3>
      <div
        className={cn(
          "text-xs font-semibold uppercase tracking-[0.1em]",
          agent.labelClass,
        )}
      >
        {agent.subtitle}
      </div>
      <p className="mt-2 text-sm leading-relaxed text-muted-foreground">
        {agent.blurb}
      </p>
    </button>
  );
}
