// How names are typeset across the projects pages.
//
// One place on purpose. These were eight separate `font-mono` classes spread
// over the feature, which is how the same project ended up sans in the
// folder rail and mono in Settings. Change a value here rather than editing
// components, and the whole feature moves together.

/** Projects, folders and "learnings" — names a person writes, so prose. */
export const LABEL_NAME_FONT = "font-sans";

/** Datapack names. The datapack detail page renders these mono because they
 *  read as identifiers there; inside projects they sit beside folder names
 *  in the same lists, so they follow the page. Set to "font-mono" to flip. */
export const DATAPACK_NAME_FONT = "font-sans";
