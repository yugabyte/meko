// Explains the closed loop before the deploy picker: Seed Context is what
// this agent's retrieval calls actually read at review time, and a
// review's own conversation feeds new memories back into the same
// datapack. Drawn as a literal ring (three arcs of one circle, not an
// approximation) so "closed loop" is geometrically true, not just implied.
//
// Tool names are Meko's real MCP surface (confirmed against the tool list,
// not invented): context_search, memory_search, knowledgebase_search.
// The review agent's own code path today calls knowledgebase_search +
// memory_search directly (or the unified contextual_prominence_search when
// that config flag is on) — context_search is shown alongside them as
// Meko's broader retrieval surface, not a claim this agent calls it too.

const CENTER = { x: 220, y: 180 };
const RADIUS = 120;
const NODE_R = 42;

// Three points 120° apart on the ring — computed, not eyeballed, so the
// connecting arcs are true arcs of the same circle.
const NODE_A = { x: 220, y: 60 }; // Seed Context — top (-90°)
const NODE_B = { x: 324, y: 240 }; // Datapack — lower-right (30°)
const NODE_C = { x: 116, y: 240 }; // Run a Review — lower-left (150°)

export function ReviewLoopDiagram({ agentDisplayName }: { agentDisplayName: string }) {
  return (
    <div className="rounded-md border border-border p-4">
      <p className="mb-1 text-sm font-medium">How {agentDisplayName} reads your datapack</p>
      <p className="mb-3 text-xs text-muted-foreground">
        A closed loop: what you seed is what gets retrieved, and every review adds back to it.
      </p>
      <svg viewBox="0 0 440 430" className="mx-auto h-auto w-full max-w-md" role="img" aria-label="Closed-loop diagram: Seed Context feeds the Datapack, which the review reads via context_search, memory_search, and knowledgebase_search, and every review writes new memories back into the Datapack.">
        <defs>
          <marker id="loopArrow" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse">
            <path d="M0,0 L10,5 L0,10 z" className="fill-primary" />
          </marker>
        </defs>

        {/* Faint backdrop ring so the 3 arcs read as one circle. */}
        <circle
          cx={CENTER.x}
          cy={CENTER.y}
          r={RADIUS}
          className="stroke-border"
          fill="none"
          strokeWidth={1}
          strokeDasharray="3 4"
        />

        {/* A -> B: Seed Context feeds the Datapack. */}
        <path
          d={`M${NODE_A.x},${NODE_A.y} A${RADIUS},${RADIUS} 0 0,1 ${NODE_B.x},${NODE_B.y}`}
          className="text-primary"
          stroke="currentColor"
          fill="none"
          strokeWidth={2}
          markerEnd="url(#loopArrow)"
        />
        {/* B -> C: the Datapack is read via Meko's retrieval tools. */}
        <path
          d={`M${NODE_B.x},${NODE_B.y} A${RADIUS},${RADIUS} 0 0,1 ${NODE_C.x},${NODE_C.y}`}
          className="text-primary"
          stroke="currentColor"
          fill="none"
          strokeWidth={2}
          markerEnd="url(#loopArrow)"
        />
        {/* C -> A: the review turn writes new memories back, closing the loop. */}
        <path
          d={`M${NODE_C.x},${NODE_C.y} A${RADIUS},${RADIUS} 0 0,1 ${NODE_A.x},${NODE_A.y}`}
          className="text-primary"
          stroke="currentColor"
          fill="none"
          strokeWidth={2}
          markerEnd="url(#loopArrow)"
        />

        {/* Arc labels */}
        <text x={372} y={93} textAnchor="middle" className="fill-muted-foreground text-[10px]">
          Upload docs, or chat
        </text>
        <text x={68} y={93} textAnchor="middle" className="fill-muted-foreground text-[10px]">
          <tspan x={68} dy={0}>Writes new</tspan>
          <tspan x={68} dy={12}>memories back</tspan>
        </text>
        <text x={220} y={355} textAnchor="middle" className="fill-muted-foreground font-mono text-[10px]">
          <tspan x={220} dy={0}>context_search</tspan>
          <tspan x={220} dy={13}>memory_search</tspan>
          <tspan x={220} dy={13}>knowledgebase_search</tspan>
        </text>

        {/* Nodes */}
        {[
          { pos: NODE_A, title: "Seed Context", subtitle: "Knowledge + chat" },
          { pos: NODE_B, title: "Datapack", subtitle: "KB + memories" },
          { pos: NODE_C, title: "Run a Review", subtitle: "The LLM call" },
        ].map((node) => (
          <g key={node.title}>
            <circle cx={node.pos.x} cy={node.pos.y} r={NODE_R} className="fill-card stroke-primary" strokeWidth={1.5} />
            <text
              x={node.pos.x}
              y={node.pos.y - 3}
              textAnchor="middle"
              className="fill-foreground text-[11px] font-semibold"
            >
              {node.title}
            </text>
            <text
              x={node.pos.x}
              y={node.pos.y + 11}
              textAnchor="middle"
              className="fill-muted-foreground text-[9px]"
            >
              {node.subtitle}
            </text>
          </g>
        ))}
      </svg>
      <p className="mt-1 text-center text-[11px] text-muted-foreground">
        <span className="font-mono">context_search</span> also spans conversation history — Meko's broader
        retrieval surface, alongside the two {agentDisplayName} calls directly.
      </p>
    </div>
  );
}
