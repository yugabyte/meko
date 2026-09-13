// Illustrative review-run rows for the dashboard preview -- same "static,
// clearly-marked sample content" convention as sampleDiff.ts and
// anthropicReferenceReview.ts. The real data-capture path (writing one row
// per review into the datapack's AMP cluster, then reading it back here)
// isn't built yet -- see chapter3-plan-a-meko.md and
// chapter3-plan-b-amp-controller.md (uploaded to the meko-labs-chapter3
// datapack) for that work. Until then, ReviewDashboardStage renders this
// fixture instead of a real query result, labeled as sample data so it's
// never mistaken for the user's own numbers.

export type SampleReviewRun = {
  id: string;
  modelLabel: string;
  inputTokens: number;
  outputTokens: number;
  estimatedCostUsd: number;
  findingsCount: number;
  criticalCount: number;
  majorCount: number;
  minorCount: number;
  createdAt: string;
};

export const SAMPLE_REVIEW_RUNS: SampleReviewRun[] = [
  {
    id: "sample-1",
    modelLabel: "Ministral 14B",
    inputTokens: 4820,
    outputTokens: 610,
    estimatedCostUsd: 0.0011,
    findingsCount: 2,
    criticalCount: 1,
    majorCount: 1,
    minorCount: 0,
    createdAt: "2026-09-03T14:12:00Z",
  },
  {
    id: "sample-2",
    modelLabel: "Nova Lite",
    inputTokens: 4790,
    outputTokens: 540,
    estimatedCostUsd: 0.0004,
    findingsCount: 1,
    criticalCount: 0,
    majorCount: 1,
    minorCount: 0,
    createdAt: "2026-09-04T09:30:00Z",
  },
  {
    id: "sample-3",
    modelLabel: "Llama 4 Maverick 17B",
    inputTokens: 4855,
    outputTokens: 705,
    estimatedCostUsd: 0.0018,
    findingsCount: 3,
    criticalCount: 1,
    majorCount: 1,
    minorCount: 1,
    createdAt: "2026-09-05T16:45:00Z",
  },
  {
    id: "sample-4",
    modelLabel: "Ministral 14B",
    inputTokens: 4810,
    outputTokens: 590,
    estimatedCostUsd: 0.0011,
    findingsCount: 2,
    criticalCount: 1,
    majorCount: 0,
    minorCount: 1,
    createdAt: "2026-09-06T11:05:00Z",
  },
  {
    id: "sample-5",
    modelLabel: "Nova Lite",
    inputTokens: 4802,
    outputTokens: 560,
    estimatedCostUsd: 0.0004,
    findingsCount: 2,
    criticalCount: 0,
    majorCount: 1,
    minorCount: 1,
    createdAt: "2026-09-07T13:20:00Z",
  },
  {
    id: "sample-6",
    modelLabel: "Ministral 14B",
    inputTokens: 4831,
    outputTokens: 615,
    estimatedCostUsd: 0.0011,
    findingsCount: 1,
    criticalCount: 0,
    majorCount: 0,
    minorCount: 1,
    createdAt: "2026-09-08T10:50:00Z",
  },
  {
    id: "sample-7",
    modelLabel: "Llama 4 Maverick 17B",
    inputTokens: 4868,
    outputTokens: 720,
    estimatedCostUsd: 0.0018,
    findingsCount: 2,
    criticalCount: 1,
    majorCount: 1,
    minorCount: 0,
    createdAt: "2026-09-09T15:40:00Z",
  },
];
