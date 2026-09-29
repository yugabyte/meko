// What a datapack is made of, as collapsed rows the reader opens if they want
// the detail. Four constructs, each in its own context colour so the row and
// the runner's right-hand pane agree about what "memory" looks like.
//
// Artifacts are deliberately absent: they exist, but nothing in this lab
// writes one, and a construct you never see fill up reads as a dead label.

import { BookOpen, GitBranch, MessageSquare, Sparkles, type LucideIcon } from "lucide-react";

import { cn } from "@/lib/utils";
import { CONTEXT_TONE, type ContextConcept } from "@/lib/contextTones";
import {
  Accordion,
  AccordionContent,
  AccordionItem,
  AccordionTrigger,
} from "@/components/ui/accordion";

type Construct = {
  id: string;
  concept: ContextConcept;
  icon: LucideIcon;
  title: string;
  /** The one line that shows while the row is closed. */
  summary: string;
  body: string;
};

const CONSTRUCTS: Construct[] = [
  {
    id: "conversations",
    concept: "conversation",
    icon: MessageSquare,
    title: "Conversations",
    summary: "Full verbatim history, auto-indexed",
    body: "Every thread an agent or a human had against this project, kept whole. Meko indexes them as they land and tiers the old ones out of hot storage, so a finding months later can still be traced to the sentence that caused it.",
  },
  {
    id: "memory",
    concept: "memory",
    icon: Sparkles,
    title: "Memory",
    summary: "Per-agent, promotable to shared",
    body: "Single facts and preferences: the rules that never made it into a document. Private to whoever added them until a human promotes one, at which point every agent on the datapack reads it.",
  },
  {
    id: "knowledge",
    concept: "knowledge",
    icon: BookOpen,
    title: "Knowledge",
    summary: "Shared, structured and unstructured",
    body: "Documents you upload. Chunked and embedded once for the whole team, so an agent asks for the one rule its task needs instead of being handed forty pages.",
  },
  {
    id: "decision-traces",
    concept: "learning",
    icon: GitBranch,
    title: "Decision traces",
    summary: "Full chain of thought, token tracking",
    body: "Which chunks and memories a query actually retrieved, what it cost, and what the agent did with them. This is what makes an answer auditable rather than something you take on faith.",
  },
];

export function DatapackConstructs() {
  return (
    <Accordion type="multiple" className="rounded-lg border border-border">
      {CONSTRUCTS.map((construct) => {
        const Icon = construct.icon;
        const tone = CONTEXT_TONE[construct.concept];
        return (
          <AccordionItem
            key={construct.id}
            value={construct.id}
            className="border-b border-border px-4 last:border-b-0"
          >
            <AccordionTrigger className="gap-3 py-3 hover:no-underline">
              <span className={cn("flex h-7 w-7 shrink-0 items-center justify-center rounded-md", tone.soft)}>
                <Icon className={cn("h-3.5 w-3.5", tone.text)} aria-hidden />
              </span>
              <span className="flex min-w-0 flex-1 flex-wrap items-baseline gap-x-2 text-left">
                <span className="text-sm font-medium text-foreground">{construct.title}</span>
                <span className="text-xs text-muted-foreground">{construct.summary}</span>
              </span>
            </AccordionTrigger>
            <AccordionContent className="pb-3 pl-10 text-sm text-muted-foreground">
              {construct.body}
            </AccordionContent>
          </AccordionItem>
        );
      })}
    </Accordion>
  );
}
