// Runs per day over the snapshot window, as a bar chart. Inline SVG rather
// than a chart library: one series, no interaction, and the axis labels are
// three fixed dates.

import { PRODUCTION_RUNS_30D } from "../../data/adlcReviewAgentLab/productionSnapshot";

const HEIGHT = 120;
const BAR_GAP = 2;

export function RunsPerDayChart() {
  const points = PRODUCTION_RUNS_30D;
  const max = Math.max(...points.map((p) => p.runs), 1);
  // A percentage-width viewBox keeps the bars proportional at any pane width
  // without measuring the container.
  const width = points.length * 10;
  const barWidth = 10 - BAR_GAP;

  return (
    <figure className="flex flex-col gap-2">
      <svg
        viewBox={`0 0 ${width} ${HEIGHT}`}
        preserveAspectRatio="none"
        className="h-32 w-full"
        role="img"
        aria-label={`Runs per day from ${points[0].day} to ${points[points.length - 1].day}, peaking at ${max} runs`}
      >
        {points.map((p, i) => {
          const h = Math.max((p.runs / max) * (HEIGHT - 4), 1);
          const last = i === points.length - 1;
          return (
            <rect
              key={p.day}
              x={i * 10}
              y={HEIGHT - h}
              width={barWidth}
              height={h}
              rx={1}
              // The final bar is a partial day, so it is drawn as one rather
              // than reading as a collapse in volume.
              className={last ? "fill-primary/30" : "fill-primary"}
            />
          );
        })}
      </svg>
      <figcaption className="flex justify-between font-mono text-[10px] text-muted-foreground">
        <span>{points[0].day}</span>
        <span>{points[Math.floor(points.length / 2)].day}</span>
        <span>{points[points.length - 1].day} (partial)</span>
      </figcaption>
    </figure>
  );
}
