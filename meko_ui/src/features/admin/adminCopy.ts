// Shared copy for the Admin console. Centralized per meko_ui/AGENTS.md: any
// string used in 2+ files is an exported const rather than an inline duplicate.

/** Appended to every admin page's "failed to load" alert — the two common
 *  causes of an admin API failure (api_server down, or a mismatched secret). */
export const ADMIN_API_ERROR_HINT =
  "Ensure api_server is running and ADMIN_API_KEY matches on both sides.";
