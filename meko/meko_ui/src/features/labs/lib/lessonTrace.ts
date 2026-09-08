/** One call a lesson made while completing -- an MCP tool call (e.g.
 *  `memory_add`, `context_search`) for lessons that go through Meko's MCP
 *  server, or a plain API call for the one lesson that doesn't (file
 *  upload). Shown in the right pane's X-Ray. */
export type XRayCallStatus = "ok" | "error" | "skipped";

export type XRayCall = {
  name: string;
  durationMs: number;
  status: XRayCallStatus;
  detail?: string;
};

/** The calls made by the most recently completed lesson, shown in X-Ray
 *  until the next lesson completes. */
export type LessonTrace = {
  lessonTitle: string;
  calls: XRayCall[];
};
