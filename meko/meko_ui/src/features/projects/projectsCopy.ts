// Shared copy and labels for the Projects surface. Anything used by two or
// more files lives here so a wording change happens in one place.

export const PROJECTS_TITLE = "Projects";

export const PROJECTS_SUBTITLE =
  "Group the datapacks that work toward one outcome. Folders inside a project hold the " +
  "standards, the work in flight, and what it taught you.";

export const NEW_PROJECT_LABEL = "New Project";

/** Scope tabs on the projects grid. Same three words the Datapacks page
 *  uses, so the two lists read as the same control. */
export const SCOPE_TAB_ALL = "All";
export const SCOPE_TAB_MINE = "Mine";
export const SCOPE_TAB_SHARED = "Shared with me";

export const PROJECTS_SEARCH_PLACEHOLDER = "Search projects...";

/** The create tile that ends the grid. */
export const NEW_PROJECT_TILE_BODY =
  "Group datapacks into folders around one outcome";

/** An example name per layout, used as the Name field's placeholder and as
 *  the name drawn in the preview before the user types one. Keyed on
 *  theme_key; an unknown layout falls back to the generic one, so adding a
 *  layout server-side never leaves a blank preview. */
const LAYOUT_EXAMPLE_NAME: Record<string, string> = {
  adlc: "adlc",
  support: "support",
};
export const GENERIC_PROJECT_EXAMPLE_NAME = "my_project";

export function layoutExampleName(themeKey: string | undefined): string {
  if (!themeKey) return GENERIC_PROJECT_EXAMPLE_NAME;
  return LAYOUT_EXAMPLE_NAME[themeKey] ?? GENERIC_PROJECT_EXAMPLE_NAME;
}

/** Empty-state copy for a workspace with no projects yet. */
export const PROJECTS_EMPTY_TITLE = "No projects yet";
export const PROJECTS_EMPTY_BODY =
  "A project groups datapacks into folders. Pick a layout to start with, then add or rename " +
  "folders as you go.";

/** Shown wherever a datapack has no placement. */
export const UNFILED_LABEL = "Unfiled";

/** Filing a datapack, from the Datapacks grid and the datapack header. "Add
 *  to" rather than "File into": it sits in a three-button strip and in a
 *  menu, and it is the plain thing a user is trying to do. */
export const ADD_TO_PROJECT_LABEL = "Add to Project";
/** Same dialog, different verb: the datapack already has a home and this
 *  changes it. Placement is an upsert keyed on the datapack, so filing and
 *  moving are the same write. */
export const MOVE_INTO_PROJECT_LABEL = "Move";

export const FOLDER_EMPTY_LABEL = "No datapacks in this folder yet";

/** Delete confirmations. Both lead with the irreversibility, because both
 *  destroy content rather than structure. */
export const DELETE_PROJECT_TITLE = "Delete this project?";
export const DELETE_PROJECT_BODY =
  "This is permanent and cannot be undone. The project, its folder structure, and every " +
  "datapack filed inside it are deleted for good, along with their memories, knowledge and " +
  "history.";

/** Appended when the project still holds datapacks, so the number is in front
 *  of the user before they type the name. */
export function deleteProjectDatapackWarning(count: number): string {
  if (count === 0) return "This project holds no datapacks.";
  return `${countLabel(count, "datapack")} will be deleted with it.`;
}

export const DELETE_FOLDER_TITLE = "Delete this folder?";
export const DELETE_FOLDER_BODY =
  "The folder is removed. Any datapacks inside become unfiled rather than being deleted, so " +
  "this is not the same as deleting the project.";

/** Pluralises a count with its noun: countLabel(1, "folder") === "1 folder".
 *  Pass `plural` for anything that does not just take an "s" ("entries"). */
export function countLabel(count: number, noun: string, plural?: string): string {
  if (count === 1) return `${count} ${noun}`;
  return `${count} ${plural ?? `${noun}s`}`;
}

// ---------------------------------------------------------------------------
// Project detail page
// ---------------------------------------------------------------------------

export const TREE_HINT = "click a row";

/** The synthetic node under a project that rolls up what its work taught you.
 *  Not a folder: nothing is filed into it. */
export const LEARNINGS_NODE_LABEL = "learnings";
export const LEARNINGS_NODE_SUBTITLE = "What the work in this project taught you";
export const LEARNINGS_NODE_BODY =
  "Memories extracted from every datapack in this project, with the ones still waiting for a " +
  "promotion decision called out.";

export const PROJECT_NODE_SUBTITLE = "The project itself";
export const PROJECT_NODE_BODY =
  "Everything in it shares one set of folders and one set of people who can reach it.";

export const FOLDER_NODE_SUBTITLE = "A folder in this project";
export const FOLDER_NODE_BODY =
  "Holds the datapacks doing similar work. The numbers below are the total across them.";

export const DATAPACK_NODE_SUBTITLE = "A datapack in this project";

/** Tabs on the project detail page. */
export const PROJECT_TAB_FOLDERS = "Folders";
export const PROJECT_TAB_ACTIVITY = "Activity";
export const PROJECT_TAB_SETTINGS = "Settings";

/** The two rules worth knowing on this page, one sentence each. */
export const PROMOTION_RULE_TITLE = "Promoted memories are shared. Private ones are not.";
export const PROMOTION_RULE_BODY =
  "A memory starts private to the datapack that made it, and promoting it publishes it so the " +
  "rest of the project can read it.";

export const READABLE_RULE_TITLE = "What a datapack here can read";
export const READABLE_RULE_BODY =
  "Its own knowledge and memories, plus anything promoted into this project's shared knowledge.";

export const STATS_TITLE = "What this project holds";

/** Knowledge files, and the qualifier that says which ones are counted:
 *  a file mid-ingest is not searchable yet, so only indexed ones count. */
export const KB_FILES_LABEL = "Knowledge files";
export const KB_FILES_INDEXED_HINT = "indexed";

// Settings tab.
export const ADD_FOLDER_LABEL = "Add folder";
/** The field that names the one folder a Custom project starts with. */
export const NEW_FOLDER_LABEL = "New Folder";
/** Says why the description is worth filling in, since it is optional and the
 *  payoff is later: search has to pick a folder, and a name alone rarely says
 *  what belongs in one. */
export const FOLDER_DESCRIPTION_HINT =
  "Helps you and your agents pick the right folder later.";
export const RENAME_FOLDER_LABEL = "Rename folder";
export const MOVE_DATAPACK_LABEL = "Move datapack";
/** The folder-contents pane's own button. "Add a datapack" rather than
 *  "File into": from inside a folder, adding to it is the plain reading. */
export const ADD_A_DATAPACK_LABEL = "Add a datapack";
export const DANGER_ZONE_TITLE = "Danger zone";
export const DANGER_ZONE_BODY =
  "Both actions are permanent and cannot be undone, and both destroy content rather than just " +
  "structure.";
export const DELETE_DATAPACK_LABEL = "Delete a datapack";
export const DELETE_PROJECT_LABEL = "Delete project";

/** Toasts for the writes on this surface. Every one of them reports, because
 *  several change something the user cannot see from where they clicked:
 *  adding a folder from the page header, moving a datapack from Settings. A
 *  silent success there reads as nothing having happened. On failure the
 *  API's own message becomes the description. */
export const PROJECT_TOASTS = {
  folderAdded: { ok: "Folder added", fail: "Could not add the folder" },
  folderRenamed: { ok: "Folder renamed", fail: "Could not rename the folder" },
  projectCreated: { ok: "Project created", fail: "Could not create the project" },
  projectDeleted: { ok: "Project deleted", fail: "Could not delete the project" },
  datapackDeleted: { ok: "Datapack deleted", fail: "Could not delete the datapack" },
  datapackFiled: { ok: "Datapack filed", fail: "Could not file the datapack" },
} as const;

/** Success title when the same write is a move rather than a first filing.
 *  Placement is one upsert, so only the wording differs. */
export const DATAPACK_MOVED_TOAST = "Datapack moved";

/** Failure description when the API sent nothing usable back. */
export const TOAST_RETRY_DESCRIPTION = "Something went wrong. Try again.";

/** The Activity tab. The feed is derived from content that still exists,
 *  not from an event log, so it is honest about the two things it covers. */
export const ACTIVITY_TAB_DESCRIPTION =
  "Files uploaded, memories promoted, folders created and datapacks filed across this " +
  "project, newest first. A move shows as a fresh filing. Renames and deletions are not " +
  "recorded, so they do not appear here.";
export const PROMOTIONS_TILE_TITLE = "Promotions";
