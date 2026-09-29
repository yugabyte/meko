import type { LabChapter } from "../../types";

// Four chapters. Chapter 1 opens on the PR itself so the user watches AgentK
// miss a real bug before building anything; Chapter 2 introduces the agent,
// hands it the datapack and configures the run; Chapter 3 is the review, from
// running it through writing its findings back and pricing what that cost.
// Chapter 4 leaves the sample behind: first Yugabyte's own production AgentK
// (a dated snapshot, see productionSnapshot.ts), then a real pull request of
// the user's own, fetched client-side with their own GitHub token
// (lib/api/githubApi.ts) and never sent to Meko.
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
        title: "Or just start a conversation",
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
      "Meet the reviewer, grant it read access to the datapack you just built, configure the run, and see what that access costs.",
    stages: [
      {
        id: "meet-agentk",
        title: "Meet AgentK",
        summary:
          "The reviewer. Open source, running on AWS AgentCore, and knowing nothing about your team until you hand it a datapack.",
      },
      {
        id: "share-with-agent",
        title: "Share the datapack with AgentK",
        summary:
          "Sharing is a grant, not a transfer. AgentK reads the live datapack, so a rule you edit at 4pm lands in the 4:01 review.",
        gate: { message: "Share the datapack to continue" },
      },
      {
        // Absorbed the old "How it reads the standards" step: choosing the
        // retrieval strategy and choosing the model are one decision, and
        // split across two screens neither could show the other's effect.
        id: "configure-agentk",
        title: "How AgentK reviews",
        summary:
          "What the agent reads, and which model reads it. Retrieval is what makes a small model enough, and this is the run Chapter 3 executes.",
      },
      {
        id: "guardrail",
        title: "Hand over the wheel",
        summary:
          "Invert the loop: the agent drives the stage and comes back at the two moments that need a human. Context is what buys that.",
      },
    ],
  },
  {
    id: "read-the-review",
    title: "Review the results",
    summary:
      "The same pull request and the same agent, now reading the context you built: the findings it returns, and what that context cost.",
    stages: [
      {
        id: "run-review",
        title: "Run the review",
        summary:
          "Same diff, same agent, the model you configured. The only difference is that AgentK can now read your datapack.",
        gate: { message: "Run the review to continue" },
      },
      {
        id: "review-findings",
        title: "The findings",
        summary:
          "Every comment names the rule it came from. Disagree with one and you fix the rule, not the prompt.",
      },
      {
        // Was Chapter 4's opener. It belongs here: the findings are what the
        // lesson before just produced, and writing them back is the same
        // breath as reading them, not a chapter of its own.
        id: "save-findings",
        title: "Extract the learning",
        summary:
          "Write this review's findings into a datapack, so the agent that fixes them starts where the reviewer stopped.",
      },
      {
        id: "review-cost",
        title: "What the context cost",
        summary:
          "Real token counts for the run you just did, and what the same review would have cost without retrieval.",
      },
      // PARKED: "Collaborate". Sharing with a teammate is the same act the
      // chapter before already performs against AgentK, so as the last lesson
      // of the review chapter it read as a repeat rather than a payoff.
      // CollaborateStage and its case in LabStageSlot are still in the tree.
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
  // REMOVED: the "Close the loop" chapter. "Save the findings" moved up into
  // Chapter 3 as "Extract the learning", and "Promote the learning" is gone:
  // promoting a private memory to the datapack is a product feature with its
  // own page, and as a lab step it asked the reader to change what other
  // people's agents read before they had any reason to.
  {
    id: "see-it-in-production",
    title: "See it in production",
    summary:
      "The same loop, running against Yugabyte's own 104 repositories, then a pull request of your own.",
    stages: [
      {
        id: "let-it-run",
        title: "Let it run",
        summary:
          "The loop you just drove by hand, running on its own against Yugabyte's code: what triggers it, and where a person still comes in.",
      },
      {
        id: "public-runs",
        title: "The public runs",
        summary:
          "Real pull requests on yugabyte/yugabyte-db that AgentK reviewed, each one linking out to the thread so you can check it yourself.",
      },
      {
        id: "read-one-yourself",
        title: "Read one for yourself",
        summary:
          "One thread end to end: what AgentK said about a line of C++, and what the engineer who got the comment wrote back.",
      },
      {
        // Absorbed the old "Thirty days of runs" step. The chart and the
        // totals it adds up to were on separate screens, so neither could be
        // read against the other.
        id: "production-reviews",
        title: "Thirty days of runs",
        summary:
          "A month of production reviews, the dashboard totals behind them, and the cost per pull request that makes reviewing all of them affordable.",
      },
      {
        id: "make-it-yours",
        title: "Make it yours",
        summary:
          "Point AgentK at a pull request of your own, fetched in your browser with your own GitHub token, then take the setup back to your work.",
      },
    ],
  },
];
