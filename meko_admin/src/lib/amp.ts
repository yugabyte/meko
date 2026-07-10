// Feature gate for the AMP controller section of the admin console.
//
// SHOW_AMP_ADMIN is a RUNTIME flag (served via /runtime-config.js), so the AMP
// registration UI can be hidden per environment without a rebuild — e.g. while
// AMP is not being shipped. Off unless explicitly "true". Separate from the
// app's SHOW_PG_AMP flag on purpose: an admin may need to register AMP before
// the end-user feature is switched on.

import { readRuntimeEnv } from "@/lib/runtimeEnv";

export function isAmpAdminEnabled(): boolean {
  return (readRuntimeEnv("SHOW_AMP_ADMIN") ?? "").trim().toLowerCase() === "true";
}
