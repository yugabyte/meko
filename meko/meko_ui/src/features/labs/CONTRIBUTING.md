# Adding a new Lab

A Lab is a `LabDefinition` object (see `types.ts`) appended to
`LAB_DEFINITIONS` in `data/labDefinitions.ts`. `LabsListPage`,
`LabDetailPage`, and `LabRunnerPage` all read from that registry — adding a
lab means adding config, not touching those pages.

## Minimum to add a lab that reuses the existing demo-app kind

1. Add `data/<yourLab>/` with your lab's chapters (`LabChapter[]`, each
   holding its own `LabStage[]`) and any fixture files the demo app needs
   (see `adlcReviewAgentLab/chapters.ts` for the pattern).
2. Add a `LabDefinition` to `data/labDefinitions.ts` and push it onto
   `LAB_DEFINITIONS`.
3. Run `npm run test -- labs` — `labDefinitions.test.ts` checks your config
   for the common mistakes (duplicate ids, empty summaries) before you get
   to manual testing.

## The 18 fixed stages, grouped into chapters

Every lab walks the same 18 stages (`LabStageId` in `types.ts`): Create
Datapack → What is context? → Build a knowledge base → Add a memory → Chat
about a preference → Deploy Agent → Share your datapack → Review the sample
PR → See Claude's reference review → Configure & run your review →
Collaborate → Why a dashboard needs real storage → Create an AMP cluster →
Run another review → See your dashboard → Choose a datapack → Share your
datapack (again) → Review a real PR. They're a closed union, not per-lab
config — a lab supplies the `title`/`summary`/`highlights` copy for each, but
the stage components themselves (`components/stages/*`) are shared across
every lab — some cover more than one stage id (`CreateDatapackStage` for
both Create Datapack and Choose a Datapack, `ShareWithAgentStage` for both
Share Your Datapack cases) via optional props rather than a separate
component per reuse. `LabStageSlot.tsx` switches on `LabStage.id` to render
the right one.

The last four (the "Build a dashboard" chapter) are gated behind
`LabChapter.requiresAmp` — `getLabDefinition` (`data/labDefinitions.ts`) drops
that whole chapter when `isAmpEnabled()` is false, rather than showing stages
that would all immediately degrade. A chapter that only makes sense with a
feature flag on should set the same field, not add its own ad hoc check in
every stage.

A `LabDefinition` groups its stages into one or more `LabChapter`s
(`chapters: LabChapter[]`, each `{ id, title, stages: LabStage[] }`) — the
grouping a lab chooses is its own. `LabStageNav` shows only the current
chapter's own stages at a time (never another chapter's, even collapsed),
with an explicit "Next Chapter" / "Previous chapter" control to move between
them — moving chapters is always a deliberate action, not a side effect of
finishing the last stage. Stage numbering ("Step N.") restarts at 1 in each
chapter. `flattenLabStages` (`lib/labChapters.ts`) gives the flat, ordered
stage list other call sites need regardless of chapters (e.g.
`LabDetailPage`'s overview timeline, which keeps continuous 1..N numbering
since it shows every stage across every chapter on one page).

## Adding a new kind of demo app

`LabDemoAppConfig` is a discriminated union on `kind`. A lab whose "Run a
Review" stage needs to do something other than review a PR needs:

1. A new `kind` value + its config fields on `LabDemoAppConfig` (`types.ts`).
2. A new component under `components/demo-apps/`.
3. A `case` in `components/stages/RunReviewStage.tsx` (or wherever the new
   kind's stage lives, if it's not review-shaped at all).

`LabRunnerPage`, `LabStageNav`, and `LabObservationSlot` don't need to
change — they're generic over any `LabDefinition`.

## What's shared vs. lab-specific

- `hooks/useLabRunnerSession.ts` resolves the datapack the user picked in
  the Create Datapack stage (never auto-created/auto-selected) and fetches
  its MCP token — reusable as-is by any `pr-review-chat`-kind lab; a new
  demo-app kind that needs different session setup should extend this hook
  rather than duplicating it.
- `components/lab-runner/LabObservationSlot.tsx` wraps
  `DatapackTokenBurnTile` + `WorkbenchLMLiveObservationPanel` — reusable by
  any lab whose demo app writes real conversations/memories into the
  selected datapack, whether via `postChat` directly or (like
  `pr-review-chat`) a deployed agent invoked through `invokeDeployment`.
- `components/lab-runner/LabStageNeedsDatapack.tsx` is the shared "pick a
  datapack first" prompt every stage but Create Datapack degrades to when
  `session.datapackId` is null.
- `components/lab-runner/LabStageNeedsDeployment.tsx` is the same pattern
  for a stage that also needs an active deployment — Run a Review, Run
  Another Review, and Review a Real PR, all gated on `session.deploymentActive`.
- `components/lab-runner/LabStageNeedsAmpCluster.tsx` is the same pattern for
  a stage that needs the datapack's AMP cluster to exist first — currently
  just See Your Dashboard, gated on `session.ampClusterId` (confirmed live via
  `hooks/useValidAmpCluster.ts`, not just trusted from persisted state).
