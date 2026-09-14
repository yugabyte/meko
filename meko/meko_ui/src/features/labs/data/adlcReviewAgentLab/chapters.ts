import type { LabChapter } from "../../types";

// Five chapters. Chapter 1 opens on the PR itself so the user watches AgentK
// miss a real bug before building anything, Chapter 2 introduces the agent
// and hands it the datapack, and Chapter 3 is the review. "Build a dashboard"
// reads that datapack's own activity back out of Meko. "Bring your own PR"
// points AgentK at a real pull request instead of the bundled sample, fetched
// client-side with the user's own GitHub token (lib/api/githubApi.ts) and
// never sent to Meko.
export const ADLC_REVIEW_AGENT_LAB_CHAPTERS: LabChapter[] = [
  {
    id: "give-context",
    title: "Give your agent context",
    summary:
      "Watch AgentK review a pull request knowing nothing about your team, then build the knowledge, memory and conversation it was missing.",
    stages: [
      {
        id: "see-the-pr",
        title: "See the pull request",
        summary:
          "Thirty-four lines, tests green, CI passing. Let AgentK review it with no context at all first.",
        gate: { message: "Run the cold review to see the baseline" },
      },
      {
        id: "create-datapack",
        title: "Create a datapack",
        summary:
          "One datapack holds the knowledge, memories and conversations AgentK will read. Pick an existing one or start fresh.",
        gate: { message: "Pick or create a datapack to continue" },
      },
      {
        id: "knowledge-base",
        title: "Load your coding standards",
        summary:
          "The rules your team already wrote down. Meko chunks and embeds them once, so an agent can ask for one rule instead of reading forty pages.",
        gate: { message: "Add a standards doc to continue" },
      },
      {
        id: "memory",
        title: "Add a memory",
        summary:
          "Every team has rules that never made it into a document. Save one directly. It's live the moment you hit save.",
        gate: { message: "Save one preference to continue" },
      },
      {
        id: "conversation",
        title: "Or just say it in passing",
        summary:
          "Talk the way you'd talk to a teammate. Meko reads the conversation and writes the memory itself.",
        gate: { message: "Send a message to continue" },
      },
    ],
  },
  {
    id: "share-with-agentk",
    title: "Share it with AgentK",
    summary:
      "Meet the reviewer, grant it read access to the datapack you just built, and see what that access costs.",
    stages: [
      {
        id: "meet-agentk",
        title: "Meet AgentK",
        summary:
          "The reviewer. Open source, running on AWS AgentCore, and knowing nothing about your team until you hand it a datapack.",
      },
      {
        id: "share-with-agent",
        title: "Share the datapack, not a copy",
        summary:
          "Sharing is a grant, not a transfer. AgentK reads the live datapack, so a rule you edit at 4pm lands in the 4:01 review.",
        gate: { message: "Share the datapack to continue" },
      },
      {
        id: "retrieval-cost",
        title: "How it reads the standards",
        summary:
          "Two ways to get a forty-page document into a model, and the token bill for each. This is the whole cost argument.",
      },
      {
        id: "guardrail",
        title: "Decide where it needs you",
        summary:
          "An autonomous agent is only useful when you know where it stops. This is the human-in-the-loop line for review.",
      },
    ],
  },
  {
    id: "read-the-review",
    title: "Read the review",
    summary:
      "The same pull request and the same agent, now reading the context you built: the findings it returns, and what that context cost.",
    stages: [
      {
        id: "run-review",
        title: "Run the review",
        summary:
          "Nothing about the diff changed. The only difference is that AgentK can now read your datapack.",
        gate: { message: "Run the review to continue" },
      },
      {
        id: "review-findings",
        title: "The findings",
        summary:
          "Every comment names the rule it came from. Disagree with one and you fix the rule, not the prompt.",
      },
      {
        id: "review-cost",
        title: "What the context cost",
        summary:
          "Real token counts for the run you just did, and what the same review would have cost without retrieval.",
      },
      {
        id: "collaborate",
        title: "Collaborate",
        summary: "Share this datapack with teammates so they see the same knowledge and memories.",
      },
    ],
  },
  // PARKED: the whole "Build a dashboard" chapter. Its dashboard read Meko's
  // own conversation history and token accounting, which now lives at the end
  // of "What the context actually cost" (DatapackActivitySummary) where the
  // cost conversation already is. "Run another review" re-ran the same review
  // against the same PR to add one row to a table that already had Chapter 3's
  // run in it, and the AMP step before it provisioned a cluster nothing read.
  // Every stage component is still in the tree; restoring the chapter is
  // uncommenting this block and its cases in LabStageSlot.
  // {
  //   id: "build-a-dashboard",
  //   title: "Build a dashboard",
  //   summary:
  //     "Read back a dashboard of this datapack's own activity, drawn from the conversations and token accounting Meko already keeps.",
  //   stages: [
  //     {
  //       id: "dashboard-intro",
  //       title: "Where a dashboard's data comes from",
  //       summary:
  //         "Meko already keeps enough about this datapack to chart it. Structured data your own app owns is a separate question.",
  //     },
  //     // PARKED: the AMP cluster step. The dashboard below reads Meko's own
  //     // conversation history and token accounting, never the cluster, so
  //     // provisioning one was a prerequisite nothing used. The stage,
  //     // CreateAmpClusterStage, useValidAmpCluster and LabStageNeedsAmpCluster
  //     // are all still in the tree for when the write path into that cluster
  //     // exists and this step earns its place again.
  //     // {
  //     //   id: "create-amp-cluster",
  //     //   title: "Create an AMP cluster",
  //     //   summary:
  //     //     "Provision a real fractional Postgres instance for this datapack. It's your app's storage, not Meko's.",
  //     //   gate: { message: "Create a cluster to continue" },
  //     // },
  //     {
  //       id: "run-review-again",
  //       title: "Run another review",
  //       summary:
  //         "One more review, so the dashboard's per-session table has more than a single row to show.",
  //       gate: { message: "Run a review to continue" },
  //     },
  //     {
  //       id: "review-dashboard",
  //       title: "See your dashboard",
  //       summary:
  //         "Conversations, memories, requests and tokens saved, read back from this datapack, plus this session's own runs.",
  //     },
  //   ],
  // },
  {
    id: "close-the-loop",
    title: "Close the loop",
    summary:
      "Point AgentK at a real pull request of your own, then take the whole setup back to your work.",
    stages: [
      {
        id: "save-findings",
        title: "Save the findings",
        summary:
          "Write this review's findings into the feature's own datapack, so the agent that fixes them starts where the reviewer stopped.",
      },
      {
        id: "bring-your-own-pr",
        title: "Review a real PR",
        summary:
          "Paste a pull request you have access to. It's fetched in your browser with your own GitHub token, which never reaches Meko.",
      },
      {
        id: "make-it-yours",
        title: "Make it yours",
        summary: "What you built, and the four ways to point the same setup at your own repositories.",
      },
    ],
  },
];
