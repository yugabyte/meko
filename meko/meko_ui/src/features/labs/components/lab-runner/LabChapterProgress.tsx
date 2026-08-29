import { Stepper } from "@/components/ui/stepper";

import type { LabChapter } from "../../types";

export function LabChapterProgress({
  chapters,
  currentChapterIndex,
}: {
  chapters: LabChapter[];
  currentChapterIndex: number;
}) {
  return (
    <Stepper steps={chapters.map((c) => c.title)} current={currentChapterIndex} className="px-4 py-3" />
  );
}
