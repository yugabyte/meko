import type { LabStage } from "../../types";

export const ADLC_REVIEW_AGENT_LAB_STAGES: LabStage[] = [
  {
    id: "create-datapack",
    title: "Create a datapack",
    summary: "Pick an existing datapack or create a new one — this lab reads and writes into it.",
    highlights: ["Pick or create one", "Everything lives here"],
  },
  {
    id: "seed-context",
    title: "Seed context",
    summary: "Upload your org's coding standards, or just tell Meko about them in a quick chat.",
    highlights: ["Upload docs or chat", "Becomes searchable knowledge"],
  },
  {
    id: "deploy-agent",
    title: "Deploy AgentK",
    summary: "Stand up AgentK on AWS Bedrock AgentCore — managed, or in your own account.",
    highlights: ["AgentCore, managed or BYOC", "Reads back from the datapack"],
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
];
