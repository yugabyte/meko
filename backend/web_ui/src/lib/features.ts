// Switches for parts of the UI that are off for now.

// Promotion shares a memory with every agent through the knowledge base: the collective memory. Meko
// local has one user on one laptop, so it adds a concept without a benefit, and the UI hides it
// everywhere (the Collective scope, Promote, the counts, the activity log's promotions, the map's
// collective dots). The server still serves it. true brings it all back.
export const SHOW_COLLECTIVE = false;
