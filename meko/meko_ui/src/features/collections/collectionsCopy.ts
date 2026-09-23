// Shared copy and labels for the Collections surface. Anything used by two or
// more files lives here so a wording change happens in one place.

export const COLLECTIONS_TITLE = "Collections";

export const COLLECTIONS_SUBTITLE =
  "Group the datapacks that work toward one outcome. Folders inside a collection hold the " +
  "standards, the work in flight, and what it taught you.";

export const NEW_COLLECTION_LABEL = "New Collection";

export const SHARED_COLLECTIONS_TITLE = "Shared collections";
/** Says what is and isn't visible, because a recipient seeing "1 datapack" in
 *  someone's collection will otherwise wonder whether that is the whole collection. */
export const SHARED_COLLECTIONS_SUBTITLE =
  "Collections belonging to other people, holding datapacks shared with you. You see only the " +
  "datapacks you have access to and the folders they sit in, not the rest of the collection.";

/** Empty-state copy for a workspace with no collections yet. */
export const COLLECTIONS_EMPTY_TITLE = "No collections yet";
export const COLLECTIONS_EMPTY_BODY =
  "A collection groups datapacks into folders. Pick a layout to start with, then add or rename " +
  "folders as you go.";

/** Shown wherever a datapack has no placement. */
export const UNFILED_LABEL = "Unfiled";

export const UNFILED_PANEL_TITLE = "Unfiled datapacks";

/** Answers the two questions a user actually has here: do I need to do
 *  something, and what happens if I do. Deliberately says nothing about "before
 *  Collections" — a version boundary is meaningful to us, not to someone looking
 *  at their own workspace. */
export const UNFILED_PANEL_BODY =
  "Datapacks that aren't in a collection. They work exactly as they always have, so filing them " +
  "is optional, and a filed datapack can be moved to another folder or collection at any time.";

export const FILE_INTO_COLLECTION_LABEL = "File into a collection";
/** Same dialog, different verb: the datapack already has a home and this
 *  changes it. Placement is an upsert keyed on the datapack, so filing and
 *  moving are the same write. */
export const MOVE_INTO_COLLECTION_LABEL = "Move";

/** Footer notes on the Datapacks table. Each explains a behaviour the table
 *  shows but cannot justify on its own. */
export const DATAPACKS_FOOTER_NOTES = [
  {
    title: "A datapack lives in one place",
    body:
      "A datapack belongs to at most one collection at a time. Moving it between folders, or to a " +
      "different collection, changes where it lives rather than copying it, so there is never a " +
      "second copy to keep in step.",
  },
  {
    title: "What “Promoted” means",
    body:
      "Counts the memories this datapack has published to its collection. Many memories with " +
      "zero promoted means work nobody else can benefit from.",
  },
] as const;

export const FOLDER_EMPTY_LABEL = "No datapacks in this folder yet";

/** Delete confirmations. Both lead with the irreversibility, because both
 *  destroy content rather than structure. */
export const DELETE_COLLECTION_TITLE = "Delete this collection?";
export const DELETE_COLLECTION_BODY =
  "This is permanent and cannot be undone. The collection, its folder structure, and every " +
  "datapack filed inside it are deleted for good, along with their memories, knowledge and " +
  "history.";

/** Appended when the collection still holds datapacks, so the number is in front
 *  of the user before they type the name. */
export function deleteCollectionDatapackWarning(count: number): string {
  if (count === 0) return "This collection holds no datapacks.";
  return `${countLabel(count, "datapack")} will be deleted with it.`;
}

export const DELETE_FOLDER_TITLE = "Delete this folder?";
export const DELETE_FOLDER_BODY =
  "The folder is removed. Any datapacks inside become unfiled rather than being deleted, so " +
  "this is not the same as deleting the collection.";

/** Pluralises a count with its noun: countLabel(1, "folder") === "1 folder".
 *  Pass `plural` for anything that does not just take an "s" ("entries"). */
export function countLabel(count: number, noun: string, plural?: string): string {
  if (count === 1) return `${count} ${noun}`;
  return `${count} ${plural ?? `${noun}s`}`;
}

// ---------------------------------------------------------------------------
// Collection detail page
// ---------------------------------------------------------------------------

export const TREE_HINT = "click a row";

/** The synthetic node under a collection that rolls up what its work taught you.
 *  Not a folder: nothing is filed into it. */
export const LEARNINGS_NODE_LABEL = "learnings";
export const LEARNINGS_NODE_SUBTITLE = "What the work in this collection taught you";
export const LEARNINGS_NODE_BODY =
  "Memories extracted from every datapack in this collection, with the ones still waiting for a " +
  "promotion decision called out.";

export const COLLECTION_NODE_SUBTITLE = "The collection itself";
export const COLLECTION_NODE_BODY =
  "Everything in it shares one set of folders and one set of people who can reach it.";

export const FOLDER_NODE_SUBTITLE = "A folder in this collection";
export const FOLDER_NODE_BODY =
  "Holds the datapacks doing similar work. The numbers below are the total across them.";

export const DATAPACK_NODE_SUBTITLE = "A datapack in this collection";

/** Tabs on the collection detail page. */
export const COLLECTION_TAB_FOLDERS = "Folders";
export const COLLECTION_TAB_ACTIVITY = "Activity";
export const COLLECTION_TAB_SETTINGS = "Settings";

/** The two rules worth knowing on this page, one sentence each. */
export const PROMOTION_RULE_TITLE = "Promoted memories are shared. Private ones are not.";
export const PROMOTION_RULE_BODY =
  "A memory starts private to the datapack that made it, and promoting it publishes it so the " +
  "rest of the collection can read it.";

export const READABLE_RULE_TITLE = "What a datapack here can read";
export const READABLE_RULE_BODY =
  "Its own knowledge and memories, plus anything promoted into this collection's shared knowledge.";

/** The collective-learning pipeline, scoped to one collection. */
/** Not "Collective learning in this collection": the rename put the same
 *  root twice in one short heading. The page already says which one. */
export const PIPELINE_TITLE = "Collective learning here";
export const PIPELINE_SUBTITLE =
  "How memory from individual datapacks becomes knowledge the whole collection can read.";

// Settings tab.
export const FOLDER_OPERATIONS_TITLE = "Folder operations";
export const ADD_FOLDER_LABEL = "Add folder";
export const RENAME_FOLDER_LABEL = "Rename folder";
export const MOVE_DATAPACK_LABEL = "Move datapack";
export const DANGER_ZONE_TITLE = "Danger zone";
export const DANGER_ZONE_BODY =
  "Both actions are permanent and cannot be undone, and both destroy content rather than just " +
  "structure.";
export const DELETE_DATAPACK_LABEL = "Delete a datapack";
export const DELETE_COLLECTION_LABEL = "Delete collection";

/** Toasts for the writes on this surface. Every one of them reports, because
 *  several change something the user cannot see from where they clicked:
 *  adding a folder from the page header, moving a datapack from Settings. A
 *  silent success there reads as nothing having happened. On failure the
 *  API's own message becomes the description. */
export const COLLECTION_TOASTS = {
  folderAdded: { ok: "Folder added", fail: "Could not add the folder" },
  folderRenamed: { ok: "Folder renamed", fail: "Could not rename the folder" },
  collectionCreated: { ok: "Collection created", fail: "Could not create the collection" },
  collectionDeleted: { ok: "Collection deleted", fail: "Could not delete the collection" },
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
  "Files uploaded and memories promoted across this collection, newest first. Other changes, such " +
  "as creating a datapack or renaming a folder, are not recorded yet and do not appear here.";
export const PROMOTIONS_TILE_TITLE = "Promotions";
