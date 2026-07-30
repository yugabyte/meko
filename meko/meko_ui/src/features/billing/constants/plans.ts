// Plan catalog — single source of truth for tier limits, prices, and feature
// lists rendered on the Plans & billing page. Mirrors the pricing tiles in
// the public marketing page so any future change here ripples to both the
// header usage bars (denominator) and the plan picker cards.
//
// Backend tie-in: `meko_system.users.user_type` (enum: free | pro | enterprise
// today) maps to one of these PlanId values. The enum will need to grow to
// match (starter / growth / scale) when the billing pipeline lands; until
// then the UI defaults to "free" via the useCurrentPlan() hook.

export type PlanId = "free" | "starter" | "growth" | "scale" | "enterprise";

/** Numeric limit OR the sentinel "custom" / "unlimited" for Enterprise. */
export type PlanLimit = number | "custom" | "unlimited";

/**
 * Per-tier metric caps shown in both the header usage bars (denominator)
 * and the plan picker rows. All sizes in MB for storage so the UI can do
 * a single formatBytes() pass.
 */
export type PlanLimits = {
  datapacks: PlanLimit;
  agentsPerPack: PlanLimit;
  collaborators: PlanLimit;
  hotStorageMB: PlanLimit;
  knowledgeMB: PlanLimit;
  memoryOpsPerMo: PlanLimit;
  mcpCallsPerMo: PlanLimit;
  convHistoryDays: PlanLimit;
  /** "24 hours" displayed for Free; stored as 1 here, special-cased at render. */
  decisionTracesDays: PlanLimit;
};

export type Plan = {
  id: PlanId;
  name: string;
  tagline: string;
  /** USD per month. null for Enterprise (shown as "Custom"). */
  priceMonthly: number | null;
  /** Free tier shows "forever"; paid show "per workspace"; enterprise "talk to us". */
  priceSubtitle: string;
  limits: PlanLimits;
  /** Bullet list shown beneath the metric grid on each plan card. */
  features: readonly string[];
};

export const PLANS: readonly Plan[] = [
  {
    id: "free",
    name: "Free",
    tagline: "Try the platform",
    priceMonthly: 0,
    priceSubtitle: "forever",
    limits: {
      datapacks: 1,
      agentsPerPack: 2,
      collaborators: 1,
      hotStorageMB: 100,
      knowledgeMB: 500,
      memoryOpsPerMo: 5_000,
      mcpCallsPerMo: 10_000,
      convHistoryDays: 7,
      // 24 hours — UI special-cases values < 1 day at render time.
      decisionTracesDays: 1,
    },
    features: ["Playground access", "Claude Code, Cursor, Codex", "Community Discord"],
  },
  {
    id: "starter",
    name: "Starter",
    tagline: "For indie devs & solo founders",
    priceMonthly: 16,
    priceSubtitle: "per workspace",
    limits: {
      datapacks: 5,
      agentsPerPack: 10,
      collaborators: 3,
      hotStorageMB: 2 * 1024,
      knowledgeMB: 10 * 1024,
      memoryOpsPerMo: 50_000,
      mcpCallsPerMo: 100_000,
      convHistoryDays: 30,
      decisionTracesDays: 7,
    },
    features: ["Basic RBAC (view / edit)", "Me → Ko promotion review", "Email support, 48h"],
  },
  {
    id: "growth",
    name: "Growth",
    tagline: "Small teams running production agents",
    priceMonthly: 64,
    priceSubtitle: "per workspace",
    limits: {
      datapacks: 25,
      agentsPerPack: 50,
      collaborators: 10,
      hotStorageMB: 20 * 1024,
      knowledgeMB: 100 * 1024,
      memoryOpsPerMo: 500_000,
      mcpCallsPerMo: 1_000_000,
      convHistoryDays: 90,
      decisionTracesDays: 30,
    },
    features: [
      "Custom RBAC roles",
      "Webhooks · hybrid query API",
      "Bring-your-own embeddings",
      "Priority Slack + email, 24h",
    ],
  },
  {
    id: "scale",
    name: "Scale",
    tagline: "Mid-size teams · no sales call required",
    priceMonthly: 256,
    priceSubtitle: "per workspace",
    limits: {
      datapacks: 100,
      agentsPerPack: 200,
      collaborators: 50,
      hotStorageMB: 100 * 1024,
      knowledgeMB: 500 * 1024,
      memoryOpsPerMo: 2_500_000,
      mcpCallsPerMo: 5_000_000,
      convHistoryDays: 180,
      // Stored as 90; the "+ export" suffix is rendered conditionally per-tier.
      decisionTracesDays: 90,
    },
    features: [
      "SSO (Google + Microsoft Entra)",
      "Team workspaces · billing roll-up",
      "Usage analytics by agent",
      "Multi-region read replicas",
      "99.5% uptime target",
    ],
  },
  {
    id: "enterprise",
    name: "Enterprise",
    tagline: "Regulated, on-prem, or very large",
    priceMonthly: null,
    priceSubtitle: "talk to us",
    limits: {
      datapacks: "unlimited",
      agentsPerPack: "unlimited",
      collaborators: "unlimited",
      hotStorageMB: "custom",
      knowledgeMB: "custom",
      memoryOpsPerMo: "custom",
      mcpCallsPerMo: "custom",
      // Configurable / 10+ years — both special-cased at render time so the
      // sentinel stays a single value here.
      convHistoryDays: "custom",
      decisionTracesDays: "custom",
    },
    features: [
      "SAML / SCIM provisioning",
      "Private cloud · air-gapped on-prem",
      "Custom SLA + credits",
      "SOC 2 · HIPAA · GDPR DPA",
      "Dedicated support engineer",
    ],
  },
] as const;

const PLAN_BY_ID: Record<PlanId, Plan> = PLANS.reduce(
  (acc, p) => {
    acc[p.id] = p;
    return acc;
  },
  {} as Record<PlanId, Plan>,
);

export function getPlan(id: PlanId): Plan {
  return PLAN_BY_ID[id];
}

/** "Most popular" callout sits above the Scale tile on the picker. */
export const MOST_POPULAR_PLAN_ID: PlanId = "scale";

/**
 * Monotonic rank used to compare tiers when deciding Upgrade vs.
 * Downgrade copy on the plan picker. Higher number = higher tier.
 * Enterprise sits above Scale even though it's quote-priced — the
 * picker uses it for the Contact-sales path, not for direct
 * comparison.
 */
export const PLAN_RANK: Record<PlanId, number> = {
  free: 0,
  starter: 1,
  growth: 2,
  scale: 3,
  enterprise: 4,
};

/**
 * Sales-contact email used for the Enterprise "Contact sales" button
 * and the bottom "Talk to sales" compliance card. One constant so the
 * address can move in one place.
 */
export const SALES_CONTACT_EMAIL = "sales@mekodata.ai";

/**
 * Rows for the "Compare every detail" section. Cell values are rendered
 * as-is; an em-dash ("—") means "not included on this tier". Groups carry
 * an uppercase section heading rendered as a sub-divider in the table.
 */
export type CompareCellValue = string;
export type CompareRow = {
  feature: string;
  values: Record<PlanId, CompareCellValue>;
};
export type CompareGroup = { heading: string; rows: readonly CompareRow[] };

export const COMPARE_GROUPS: readonly CompareGroup[] = [
  {
    heading: "Platform",
    rows: [
      {
        feature: "RBAC",
        values: {
          free: "—",
          starter: "Basic",
          growth: "Custom roles",
          scale: "Custom roles",
          enterprise: "Custom roles",
        },
      },
      {
        feature: "SSO",
        values: {
          free: "—",
          starter: "—",
          growth: "—",
          scale: "Google + Entra",
          enterprise: "SAML / SCIM",
        },
      },
      {
        feature: "Audit log export",
        values: { free: "—", starter: "—", growth: "—", scale: "✓", enterprise: "✓" },
      },
      {
        feature: "Webhooks",
        values: {
          free: "—",
          starter: "—",
          growth: "✓",
          scale: "✓ + Zapier/n8n",
          enterprise: "✓ + custom",
        },
      },
      {
        feature: "Hybrid query API",
        values: { free: "—", starter: "—", growth: "✓", scale: "✓", enterprise: "✓" },
      },
      {
        feature: "BYO embeddings",
        values: {
          free: "—",
          starter: "—",
          growth: "✓",
          scale: "✓",
          enterprise: "✓ + fine-tuned",
        },
      },
      {
        feature: "Team workspaces",
        values: { free: "—", starter: "—", growth: "—", scale: "✓", enterprise: "✓" },
      },
      {
        feature: "Usage analytics",
        values: { free: "—", starter: "—", growth: "—", scale: "✓", enterprise: "✓" },
      },
      {
        feature: "Multi-region",
        values: {
          free: "—",
          starter: "—",
          growth: "—",
          scale: "Read replicas",
          enterprise: "Full",
        },
      },
    ],
  },
  {
    heading: "Deployment & support",
    rows: [
      {
        feature: "Deployment",
        values: {
          free: "Managed",
          starter: "Managed",
          growth: "Managed",
          scale: "Managed",
          enterprise: "Private / on-prem",
        },
      },
      {
        feature: "SLA",
        values: {
          free: "—",
          starter: "—",
          growth: "—",
          scale: "99.5% target",
          enterprise: "Custom + credits",
        },
      },
      {
        feature: "Support",
        values: {
          free: "Discord",
          starter: "Email 48h",
          growth: "Slack 24h",
          scale: "Priority 8h",
          enterprise: "Dedicated engineer",
        },
      },
      {
        feature: "Compliance",
        values: {
          free: "—",
          starter: "—",
          growth: "—",
          scale: "—",
          enterprise: "SOC 2 · HIPAA · GDPR",
        },
      },
    ],
  },
] as const;
