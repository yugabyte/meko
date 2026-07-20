// Centralised navigation model for the sidebar. Add/rename/reorder nav
// here and both the sidebar and the route table stay in sync — nothing
// is hard-coded in the Sidebar component itself.

import {
  Activity,
  Boxes,
  Crosshair,
  Globe,
  Home,
  // Layers, // used only by the Foundations tab, which is commented out below
  ListChecks,
  Server,
  Sparkles,
  Workflow,
  type LucideIcon,
} from "lucide-react";
import type { ReactNode } from "react";

export type NavItem = {
  label: string;
  icon: LucideIcon;
  /** Route target. Omitted for items that open a side panel instead. */
  to?: string;
  /** When set, clicking opens this agent's side panel instead of routing. */
  panelId?: AgentPanelId;
  /** Render the teal "online" status dot (agents). */
  online?: boolean;
};

/** WORKSPACE section. */
export const WORKSPACE_NAV: NavItem[] = [
  { label: "Home", to: "/home", icon: Home },
  // Foundations tab temporarily disabled — no backing API yet.
  // { label: "Foundations", to: "/foundations", icon: Layers },
  { label: "Shared pools", to: "/shared-pools", icon: Boxes },
  { label: "Fleet", to: "/fleet", icon: Server },
  { label: "Cluster", to: "/multitenancy", icon: Globe },
  { label: "Tasks", to: "/tasks", icon: ListChecks },
];

/** AGENTS · MCP section. Architect routes to its own page; the other
 *  three open a right-hand side panel from anywhere in the app. */
export const AGENT_NAV: NavItem[] = [
  { label: "Architect", to: "/agents/architect", icon: Sparkles, online: true },
  { label: "Voyager", panelId: "voyager", icon: Crosshair, online: true },
  { label: "Perf Advisor", panelId: "perf-advisor", icon: Activity, online: true },
  { label: "Nexus", panelId: "nexus", icon: Workflow, online: true },
];

/** Nav targets that map to a route (used to build the router so routes
 *  can't fall out of sync with the sidebar). Panel-only items excluded. */
export const ROUTABLE_NAV: NavItem[] = [...WORKSPACE_NAV, ...AGENT_NAV].filter(
  (item) => item.to,
);

/* ====================================================================
 * Agent side panels
 * ==================================================================== */

export type AgentPanelId = "voyager" | "perf-advisor" | "nexus";

export type AgentPanelConfig = {
  id: AgentPanelId;
  name: string;
  subtitle: string;
  label: string;
  icon: LucideIcon;
  /** Tailwind classes for the coloured icon tile. */
  tileClass: string;
  /** Tailwind text colour for the agent label. */
  labelClass: string;
  description: ReactNode;
  /** Primary CTA caption. The button is rendered disabled for now. */
  cta: string;
  /** Short one-line summary for the Home agent card (the panel uses the
   *  richer `description`). */
  blurb: string;
  /** Render a "NEW" badge on the Home agent card. */
  isNew?: boolean;
};

/** Brand highlight used inside agent descriptions. */
function Hl({ children }: { children: ReactNode }) {
  return <span className="font-semibold text-brand">{children}</span>;
}

export const AGENT_PANELS: Record<AgentPanelId, AgentPanelConfig> = {
  voyager: {
    id: "voyager",
    name: "Voyager",
    subtitle: "Migration agent",
    label: "VOYAGER",
    icon: Crosshair,
    tileClass: "bg-teal-300 text-teal-950",
    labelClass: "text-teal-600 dark:text-teal-400",
    description: (
      <>
        I migrate data &amp; databases <Hl>in bulk</Hl> into a shared pool —
        Oracle (via <Hl>HexaBridge</Hl>), SQL Server (via <Hl>Babelfish</Hl>),
        MySQL or Postgres. I assess each source, flag incompatibilities, run
        low-downtime cutovers, and track the detail of every migration.
      </>
    ),
    cta: "Start bulk migration",
    blurb:
      "Migrates data & databases in bulk into a shared pool and tracks the detail of every migration.",
  },
  "perf-advisor": {
    id: "perf-advisor",
    name: "Perf Advisor",
    subtitle: "Tuning & troubleshooting",
    label: "PERF ADVISOR",
    icon: Activity,
    tileClass: "bg-violet-300 text-violet-950",
    labelClass: "text-violet-600 dark:text-violet-400",
    description: <>I tune and troubleshoot the whole fleet across your pools.</>,
    cta: "Apply",
    blurb:
      "Watches the fleet for hot tenants and slow queries; recommends index, placement & replication fixes.",
  },
  nexus: {
    id: "nexus",
    name: "Nexus",
    subtitle: "Solution architect · integrations",
    label: "NEXUS",
    icon: Workflow,
    tileClass: "bg-green-300 text-green-950",
    labelClass: "text-green-600 dark:text-green-400",
    description: (
      <>
        I design how this platform plugs into your wider ecosystem —
        connectors, event streams, BI tools and downstream apps. Tell me the
        systems you run and I&apos;ll propose the integration architecture (all
        wired over MCP / API).
      </>
    ),
    cta: "Draft integration plan",
    blurb:
      "Designs ecosystem & external integrations — connectors, event streams, BI tools and downstream apps.",
    isNew: true,
  },
};

/** Legend rows for the "Platform model" box pinned under the agents
 *  section. `swatch` is a Tailwind bg-* class for the colour chip. */
export type PlatformModelItem = {
  label: string;
  meta: string;
  swatch: string;
};

export const PLATFORM_MODEL: PlatformModelItem[] = [
  { label: "Foundation", meta: "storage + topology", swatch: "bg-blue-500" },
  { label: "Shared pool", meta: "team / BU", swatch: "bg-teal-400" },
  { label: "Cluster", meta: "workload", swatch: "bg-brand" },
];
