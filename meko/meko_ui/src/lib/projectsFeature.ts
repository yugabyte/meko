// Feature gate for Projects & folders — the /projects tree, its sidebar entry,
// and the project/path additions to the Datapacks page.
//
// Two layers, composed:
//
//  1. MEKO_ENABLE_PROJECTS — a RUNTIME flag served via /runtime-config.js, so
//     it flips per deployment without a rebuild, the same way MEKO_ENABLE_LABS
//     does. Off unless explicitly enabled, so a deployment that has not set it
//     gets no Projects.
//  2. ?projects=true|false — a per-browser override of that default, latched in
//     localStorage. This is the toggle: it works in both directions, needs no
//     deploy and no devtools, and survives reloads.
//
// Turning the feature off is safe at any point. meko_system.datapacks is not
// touched by this feature, so with the gate off the Datapacks page behaves
// exactly as it did before Projects existed; the projects, project_folders and
// datapack_placements rows simply stop being read, and reappear intact when it
// is turned back on.
//
// NOTE: this gates the UI only. It does not reverse the api_server rule that a
// NEW datapack must be created inside a folder — that invariant needs its own
// backend flag.

import { useFeatureFlag } from "@/lib/useFeatureFlag";
import { readRuntimeFlag } from "@/lib/runtimeEnv";

/** Runtime env var name. Same spelling the BE uses, so one value in a
 *  ConfigMap means the same thing to every service. */
export const PROJECTS_RUNTIME_FLAG = "MEKO_ENABLE_PROJECTS";

/** The deployment-level half of the gate, without the per-browser override.
 *  Use `useProjectsEnabled` in components; this is for non-React callers
 *  (route guards, plain helpers) that have no hook context. */
export function isProjectsEnvEnabled(): boolean {
  return readRuntimeFlag(PROJECTS_RUNTIME_FLAG);
}

/** The gate components should read: the deployment flag, overridden by
 *  `?projects=true|false` when this browser has made a choice. */
export function useProjectsEnabled(): boolean {
  return useFeatureFlag("projects", isProjectsEnvEnabled());
}
