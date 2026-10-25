// Switches for parts of the UI that are off for now.

// Memory review and sharing: a new memory is a learning until it's kept (reviewed) or promoted to
// the collective memory every agent finds through the knowledge base. Meko local has one user on one
// laptop, so neither step has a benefit, and the UI shows memories as just memories: no Learnings or
// Collective scope, Keep or Promote, their counts, the ribbon and breakdown built on them, the
// activity log's promotions, or state colors on the map. The server still serves all of it. true
// brings it all back.
export const SHOW_COLLECTIVE = false;
