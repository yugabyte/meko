import type { LabChapter } from "../../types";

// Two chapters per claude_plans/labs-redesign-and-chapter-1.md's target
// design: "Setup your context" builds the datapack + seeded knowledge
// AgentK will review against, "Deploy an agent" gets AgentK running
// (shared instance or your own dedicated one), shares the datapack with
// it, then runs a review.
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
        id: "run-review",
        title: "Run a review",
        summary: "Review a real pull request, or the bundled sample diff, against your seeded context.",
        highlights: ["Real PR or sample diff", "Tune tone & strictness"],
      },
      {
        id: "collaborate",
        title: "Collaborate",
        summary: "Share this datapack with teammates so they see the same knowledge and memories.",
        highlights: ["Share with teammates", "Same context for everyone"],
      },
    ],
  },
];
