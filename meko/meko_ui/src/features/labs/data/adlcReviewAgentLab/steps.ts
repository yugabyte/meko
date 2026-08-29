import type { LabChapter } from "../../types";

export const ADLC_REVIEW_AGENT_LAB_CHAPTERS: LabChapter[] = [
  {
    id: "setup",
    title: "Setup",
    steps: [
      {
        id: "load-standards",
        title: "Load your org's coding standards",
        body: "The review agent grounds every review in real context, not just the diff. Load a sample coding-standards doc into this lab's knowledge base so the agent has something to check the PR against.",
        onEnterAction: "seedKnowledge",
      },
      {
        id: "connect-github",
        title: "Connect a real PR (optional)",
        body: "Paste a GitHub personal access token and a `owner/repo#number` to review a real pull request. Skip this to review the bundled sample diff instead — either way, the agent runs the same review.",
      },
    ],
  },
  {
    id: "review",
    title: "Run the review",
    steps: [
      {
        id: "run-review",
        title: "Review the PR",
        body: "The agent fetches the diff, retrieves your coding standards from the knowledge base, and reviews the change — flagging anything that violates a documented standard and explaining why.",
      },
      {
        id: "ask-followup",
        title: "Ask a follow-up",
        body: "The chat stays open after the review. Ask the agent to explain a specific comment, or to check the diff against a different concern.",
      },
    ],
  },
  {
    id: "observe",
    title: "See what Meko did",
    steps: [
      {
        id: "watch-memory",
        title: "Watch memories get extracted",
        body: "Every turn in this chat is a real conversation against your datapack. Watch the Live observation panel on the right — new memories and promoted knowledge appear there within seconds of the agent responding, the same panel WorkbenchLM uses.",
      },
    ],
  },
];
