// What Knowledge and Artifacts each mean, for the info tips on their tiles.
//
// They live together because they are a contrasting PAIR: the tiles sit side
// by side and the only reason either sentence exists is to tell them apart.
// Editing one in isolation is how the distinction goes blurry, so both are
// here where a change to one puts the other in front of you.
//
// The line that matters is whether the CONTENTS are indexed. Both say
// "searchable" carefully: artifacts can be found by filename on the Artifacts
// page, so a flat "not searchable" here would contradict the page one click
// away. What you cannot do is search inside them.

export const KNOWLEDGE_TILE_HINT =
  "Reference material you add to this datapack: docs, notes, anything an agent should be " +
  "able to look things up in. Files are indexed once processed, so an agent can search " +
  "them by meaning and pull back just the passages that matter.";

export const ARTIFACTS_TILE_HINT =
  "Files your agents produce while they work, such as generated code, logs or a report. " +
  "They are kept exactly as written and can be downloaded, and you can find one by " +
  "filename. Their contents are not indexed, so an agent cannot search inside them.";
