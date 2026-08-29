# Adding a new Lab

A Lab is a `LabDefinition` object (see `types.ts`) appended to
`LAB_DEFINITIONS` in `data/labDefinitions.ts`. `LabsListPage`,
`LabDetailPage`, and `LabRunnerPage` all read from that registry — adding a
lab means adding config, not touching those pages.

## Minimum to add a lab that reuses the existing demo-app kind

1. Add `data/<yourLab>/` with your lab's stage content (`LabStage[]`) and
   any fixture files the demo app needs (see `adlcReviewAgentLab/stages.ts`
   for the pattern).
2. Add a `LabDefinition` to `data/labDefinitions.ts` and push it onto
   `LAB_DEFINITIONS`.
3. Run `npm run test -- labs` — `labDefinitions.test.ts` checks your config
   for the common mistakes (duplicate ids, empty summaries) before you get
   to manual testing.

## The 5 fixed stages

Every lab walks the same 5 stages (`LabStageId` in `types.ts`): Create
Datapack → Seed Context → Deploy Agent → Run a Review → Collaborate. They're
a closed union, not per-lab config — a lab supplies the `title`/`summary`/
`highlights` copy for each, but the stage components themselves
(`components/stages/*`) are shared across every lab. `LabStageSlot.tsx`
switches on `LabStage.id` to render the right one.

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
  for a stage that also needs an active deployment — currently just
  Run a Review, gated on `session.deploymentActive`.
