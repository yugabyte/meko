# Adding a new Lab

A Lab is a `LabDefinition` object (see `types.ts`) appended to
`LAB_DEFINITIONS` in `data/labDefinitions.ts`. `LabsListPage`,
`LabDetailPage`, and `LabRunnerPage` all read from that registry — adding a
lab means adding config, not touching those pages.

## Minimum to add a lab that reuses the existing demo-app kind

1. Add `data/<yourLab>/` with your lab's steps content (`LabChapter[]`) and
   any fixture files the demo app needs (see `adlcReviewAgentLab/` for the
   pattern — a bundled sample file synthesized as a `File` on demand).
2. Add a `LabDefinition` to `data/labDefinitions.ts` and push it onto
   `LAB_DEFINITIONS`.
3. Run `npm run test -- labs` — `labDefinitions.test.ts` checks your config
   for the common mistakes (duplicate ids, empty chapters/steps, a blank
   `datapackName`) before you get to manual testing.

## Adding a new kind of demo app

`LabDemoAppConfig` is a discriminated union on `kind`. A lab whose middle
pane needs to do something other than review a PR needs:

1. A new `kind` value + its config fields on `LabDemoAppConfig` (`types.ts`).
2. A new component under `components/demo-apps/`.
3. A `case` in `components/lab-runner/LabDemoAppSlot.tsx`.

`LabRunnerPage`, `LabStepsRail`, and `LabObservationSlot` don't need to
change — they're generic over any `LabDefinition`.

## What's shared vs. lab-specific

- `hooks/useLabRunnerSession.ts` resolves-or-creates a persistent datapack
  per `demoApp.datapackName` and fetches its MCP token — reusable as-is by
  any `pr-review-chat`-kind lab; a new demo-app kind that needs different
  session setup should extend this hook rather than duplicating it.
- `components/lab-runner/LabObservationSlot.tsx` wraps
  `WorkbenchLMLiveObservationPanel` — reusable by any lab whose demo app is
  a real Meko conversation (i.e. calls `postChat` with a `datapack_id`).
