import type { LabChapter } from "../../types";

// "Setup your context" builds the datapack + seeded knowledge AgentK will
// review against; "Deploy an agent" gets AgentK running (shared instance or
// your own dedicated one), shares the datapack with it, then runs a review;
// "Build a dashboard" provisions a real per-datapack AMP Postgres cluster,
// and previews what a review-run dashboard backed by it would look like. The
// dashboard's data-capture path (writing a real row per review into the AMP
// cluster) isn't wired up yet -- see the review-dashboard stage's own
// comment -- so today it renders illustrative sample data, never a live
// query, regardless of whether a cluster exists. requiresAmp on this chapter
// (below) means getLabDefinition drops it entirely when AMP isn't enabled,
// rather than showing four stages that all immediately degrade.
export const ADLC_REVIEW_AGENT_LAB_CHAPTERS: LabChapter[] = [
  {
    id: "setup-context",
    title: "Setup your context",
    summary:
      "Build your org's coding standards into a Meko datapack — using the knowledge base, memory, and conversation primitives — so AgentK has something real to review against.",
    stages: [
      {
        id: "create-datapack",
        title: "Create a datapack",
        summary: "Pick an existing datapack or create a new one — this lab reads and writes into it.",
        highlights: ["Pick or create one", "Everything lives here"],
      },
      {
        id: "context-intro",
        title: "What is context?",
        summary: "Meko builds context from three sources — you'll add one of each next.",
        highlights: ["Knowledge, memory, conversation", "Powers every later review"],
      },
      {
        id: "knowledge-base",
        title: "Build a knowledge base",
        summary: "Upload your org's coding standards, or start from our sample — it becomes searchable knowledge.",
        highlights: ["Upload docs or use ours", "Becomes searchable knowledge"],
      },
      {
        id: "memory",
        title: "Add a memory",
        summary: "Tell Meko a coding preference directly — no chat needed, it's remembered instantly.",
        highlights: ["One fact, saved instantly", "No conversation required"],
      },
      {
        id: "conversation",
        title: "Chat about a preference",
        summary: "Mention a coding preference in a normal chat — Meko extracts and remembers it automatically.",
        highlights: ["Just talk naturally", "Meko extracts what matters"],
      },
    ],
  },
  {
    id: "deploy-an-agent",
    title: "Deploy an agent",
    summary: "Get AgentK running, share what it needs to see, then watch it review against the context you built.",
    stages: [
      {
        id: "deploy-agent",
        title: "Deploy AgentK",
        summary: "Use the shared AgentK instance instantly, or deploy your own dedicated one on AWS Bedrock AgentCore.",
        highlights: ["Shared instance, no wait", "Or deploy your own"],
      },
      {
        id: "share-with-agent",
        title: "Share your datapack",
        summary: "AgentK reads under its own account — share the datapack you built so it can see it.",
        highlights: ["One-time share", "Needed for the shared instance"],
      },
      {
        id: "review-sample-pr",
        title: "Review the sample PR",
        summary: "See exactly what AgentK is about to review — a small, clearly-marked sample pull request.",
        highlights: ["A real diff, not a description", "Marked as sample content"],
      },
      {
        id: "claude-reference",
        title: "See Claude's reference review",
        summary: "Claude Sonnet 4.5 already reviewed this PR against your org's coding standards — see what it found first.",
        highlights: ["Grounded in your knowledge base", "The benchmark your model is compared to"],
      },
      {
        id: "run-review",
        title: "Configure & run your review",
        summary: "Pick a smaller model, tune tone & strictness, then run it against the same context Claude used.",
        highlights: ["Choose your own model", "Tune tone & strictness"],
      },
      {
        id: "collaborate",
        title: "Collaborate",
        summary: "Share this datapack with teammates so they see the same knowledge and memories.",
        highlights: ["Share with teammates", "Same context for everyone"],
      },
    ],
  },
  {
    id: "build-a-dashboard",
    title: "Build a dashboard",
    summary:
      "Every datapack can back its own app storage — provision a real fractional Postgres instance with AMP, then preview what a review-run dashboard backed by it would look like.",
    requiresAmp: true,
    stages: [
      {
        id: "dashboard-intro",
        title: "Why a dashboard needs real storage",
        summary: "Meko's memory and conversation store isn't built for structured, queryable app data — a dashboard needs a real database.",
        highlights: ["Structured, not free-text", "Every datapack can have one"],
      },
      {
        id: "create-amp-cluster",
        title: "Create an AMP cluster",
        summary: "Provision a real fractional Postgres instance for this datapack — this is your own app's storage, not Meko's.",
        highlights: ["A real Postgres database", "Yours, not shared"],
      },
      {
        id: "run-review-again",
        title: "Run another review",
        summary: "Run one more review — a second example to look at once you get to the dashboard preview.",
        highlights: ["Same review flow as before", "Not recorded anywhere yet"],
      },
      {
        id: "review-dashboard",
        title: "See your dashboard",
        summary: "A preview of what your review-run dashboard will look like — models used, tokens, cost, and findings.",
        highlights: ["Stat tiles + a trend chart", "Sample data, not yet live"],
      },
    ],
  },
];
