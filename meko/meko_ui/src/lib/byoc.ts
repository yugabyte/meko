// Feature gate for BYOC (bring-your-own-cloud) deployments — a data plane the
// customer runs in their own account.
//
// A RUNTIME flag served via /runtime-config.js, so it flips per deployment
// without a rebuild, the same way MEKO_ENABLE_LABS does. Off unless explicitly
// enabled, so the hosted deployments every existing customer is on keep the
// surfaces they have today and only a BYOC install opts out.
//
// What it hides is billing surface: a BYOC customer pays for their own
// infrastructure under a contract negotiated off the product, so plans and
// prices quoted in-app are not what they owe.

import { readRuntimeEnv } from "@/lib/runtimeEnv";

/** Accepts the same spellings as lib/labs.ts and the MCP server.
 *
 *  NOT identical to api_server, which parses this env var with Go's
 *  strconv.ParseBool: that takes 1/t/T/true/TRUE/True (and the false forms) but
 *  REJECTS "yes", falling back to off. So MEKO_ENABLE_BYOC=yes hides the
 *  billing surfaces here while the api-server keeps creating 'free' users.
 *  Use "true" or "1"; the chart only ever writes "true"/"false". */
export function isByocEnabled(): boolean {
  return ["1", "true", "yes"].includes(
    (readRuntimeEnv("MEKO_ENABLE_BYOC") ?? "").trim().toLowerCase(),
  );
}

/** WorkbenchLM is hosted-only: on BYOC its MCP calls carry the user's session
 *  token, which a BYOC MCP server (PAT-only) rejects, so the page cannot work. */
export function isWorkbenchLmEnabled(): boolean {
  return !isByocEnabled();
}
