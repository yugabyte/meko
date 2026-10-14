// Feature gate for datapack share links. The UI reads MEKO_ENABLE_LINK_SHARING at
// runtime; api_server reads its own copy, and both must be set (in Helm, two values).

import { readRuntimeFlag } from "@/lib/runtimeEnv";

export const LINK_SHARING_RUNTIME_FLAG = "MEKO_ENABLE_LINK_SHARING";

/** Off unless the deployment sets the flag. There is no per-browser override:
 *  with the flag off the API has no share routes to call. */
export function isLinkSharingEnabled(): boolean {
  return readRuntimeFlag(LINK_SHARING_RUNTIME_FLAG);
}

/** Absolute URL of a share link, as the owner copies it. */
export function shareLinkUrl(path: string): string {
  return `${window.location.origin}${path}`;
}
