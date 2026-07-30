// Feature gate for the AMP (Postgres databases) UI inside a datapack.
//
// SHOW_PG_AMP is a RUNTIME flag (served via /runtime-config.js), so it can be
// flipped per environment without a rebuild. Off unless explicitly "true".

import { readRuntimeEnv } from "@/lib/runtimeEnv";

export function isAmpEnabled(): boolean {
  return (readRuntimeEnv("SHOW_PG_AMP") ?? "").trim().toLowerCase() === "true";
}
