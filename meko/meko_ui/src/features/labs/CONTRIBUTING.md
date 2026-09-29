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
3. Run `npm run test -- labs` — `tests/data/labDefinitions.test.ts` checks
   your config for the common mistakes (duplicate ids, empty summaries)
   before you get to manual testing.

## Where the tests live

Under `tests/`, mirroring the tree they cover: a test for
`components/stages/Foo.tsx` is `tests/components/stages/Foo.test.tsx`.

This is deliberately unlike the rest of `meko_ui`, which co-locates its
tests. Labs is the one feature large enough for the stage directories to be
hard to read with a `.test.tsx` beside every component. Tests import their
subject through the `@/` alias rather than a relative path, so a file can
move without its test needing an edit.

## The fixed stages, grouped into chapters

`LabStageId` (`types.ts`) is a closed union, not per-lab config — a lab
supplies the `title`/`summary` copy for each, but the stage components
(`components/stages/*`) are shared across every lab. The ADLC review lab
walks 18 of them:

| Chapter | Stages |
|---|---|
| Give your agent context | `see-the-pr` → `create-datapack` → `knowledge-base` → `memory` → `conversation` |
| Share it with AgentK | `meet-agentk` → `share-with-agent` → `configure-agentk` → `guardrail` |
| Review the results | `run-review` → `review-findings` → `save-findings` → `review-cost` |
| See it in production | `let-it-run` → `public-runs` → `read-one-yourself` → `production-reviews` → `make-it-yours` |

The union also carries `collaborate`, plus `dashboard-intro`,
`create-amp-cluster`, `run-review-again` and `review-dashboard`. The last
four belong to the parked "Build a dashboard" chapter: their stage components are still in the tree, and
restoring the chapter means uncommenting its block in
`adlcReviewAgentLab/chapters.ts` and the matching cases in `LabStageSlot`.
See that file's own comment for why it was parked.

`LabStageSlot.tsx` switches on `LabStage.id` to render the right component.
A component can cover more than one stage id through optional props rather
than a copy per reuse: `CreateDatapackStage` takes `heading`/`description`
overrides for a second "pick a datapack again" placement, which no chapter
uses today.

One stage hosts another. `make-it-yours` renders `BringYourOwnPrStage` inside
its own section rather than that flow having a step of its own, so the
datapack and deployment gating is reused instead of repeated. A stage that
invokes the agent must also be listed in `STAGES_NEEDING_TOKEN`
(`pages/LabRunnerPage.tsx`), or it renders before the MCP token lands and
tells the user to deploy an agent that is already deployed.

`LabChapter.requiresAmp` drops a whole chapter when `isAmpEnabled()` is false
(`getLabDefinition`, `data/labDefinitions.ts`), rather than showing stages
that would all immediately degrade. A chapter that only makes sense with a
feature flag on should set that field, not add its own ad hoc check in every
stage.

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
