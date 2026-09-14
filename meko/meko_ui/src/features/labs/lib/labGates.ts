// Which lessons a datapack's current contents already satisfy.
//
// A lesson's gate normally opens when the lesson itself reports a trace. That
// alone strands anyone whose datapack ALREADY holds what the lesson produces
// — an earlier run, a datapack shared with them, or a reload part-way through
// — because the event has been and gone and the only way to re-fire it is to
// do the work twice. These rules are the second way in.

import type { LabContextFeed } from "../hooks/useLabContextFeed";
import type { LabStageId } from "../types";

export function stagesSatisfiedByContext(
  feed: LabContextFeed,
  datapackId: string | null,
): LabStageId[] {
  const satisfied: LabStageId[] = [];
  // A datapack is already chosen, so asking the user to choose it again to
  // unlock Next would be asking them to redo something that is plainly done.
  if (datapackId) satisfied.push("create-datapack");
  if (feed.knowledgeTotal > 0) satisfied.push("knowledge-base");
  if (feed.memoriesTotal > 0) satisfied.push("memory");
  if (feed.conversationsTotal > 0) satisfied.push("conversation");
  return satisfied;
}
