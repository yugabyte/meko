// Single tab trigger in the invite card's three-up header
// (Human · Group · Agent). Keeps the tab strip in DatapackSharePage
// declarative — the page lists three <InviteModeTab> lines instead of
// three near-identical ~35-line Tailwind blobs.
//
// `activeTone` picks the underline / tint colour of the ACTIVE state.
// Two tones today ("brand" for human paths, "violet" for agents) — the
// component owns the mapping so the tab strip doesn't have to think
// about token combinations. If a new invite path lands (Slack? SCIM?)
// add a case here rather than sprinkling classnames across the page.
import type { ComponentType, ReactNode } from "react";
import { TabsTrigger } from "@/components/ui/tabs";
import { cn } from "@/lib/utils";

/** Full-width segmented tab: equal columns, centered label, bottom rule spans the whole cell. */
const inviteTabTriggerClass = cn(
  "relative flex w-full min-w-0 min-h-[3rem] items-center justify-center gap-0 whitespace-normal rounded-none border-0 border-b-2 border-border bg-transparent px-3 py-3 text-sm font-normal leading-normal shadow-none ring-0 ring-offset-0 transition-colors",
  "hover:bg-muted/25 focus-visible:z-10 focus-visible:ring-2 focus-visible:ring-blue-500 focus-visible:ring-offset-2 focus-visible:ring-offset-background",
  "data-[state=active]:text-inherit data-[state=active]:shadow-none data-[state=active]:ring-0 data-[state=active]:ring-offset-0",
  "data-[state=inactive]:text-muted-foreground data-[state=inactive]:opacity-90 data-[state=inactive]:hover:bg-muted/20 data-[state=inactive]:hover:opacity-100",
);

export type InviteModeTabTone = "brand" | "violet";

const TONE_ACTIVE_CLASS: Record<InviteModeTabTone, string> = {
  brand:
    "data-[state=active]:border-b-brand data-[state=active]:bg-brand/10 data-[state=active]:text-foreground",
  violet:
    "data-[state=active]:border-b-violet-500 data-[state=active]:bg-violet-100 dark:data-[state=active]:bg-violet-950/20 data-[state=active]:text-foreground",
};

export type InviteModeTabProps = {
  value: string;
  icon: ComponentType<{ className?: string; "aria-hidden"?: boolean }>;
  /** Bold segment before the "·" divider — e.g. "Invite a human". */
  primaryLabel: ReactNode;
  /** Muted segment after the divider — e.g. "by email". */
  secondaryLabel: ReactNode;
  activeTone: InviteModeTabTone;
  /** Right-hand border between tabs; omit on the last tab. */
  withDivider?: boolean;
};

export function InviteModeTab({
  value,
  icon: Icon,
  primaryLabel,
  secondaryLabel,
  activeTone,
  withDivider,
}: InviteModeTabProps) {
  return (
    <TabsTrigger
      value={value}
      className={cn(
        inviteTabTriggerClass,
        "group",
        withDivider && "border-r border-border/70",
        TONE_ACTIVE_CLASS[activeTone],
      )}
    >
      <span className="inline-flex max-w-full items-center justify-center gap-2 sm:gap-2.5">
        <Icon
          className="block size-4 shrink-0 text-muted-foreground group-data-[state=active]:text-brand"
          aria-hidden
        />
        <span className="inline-flex min-h-[1.25rem] max-w-full flex-wrap items-baseline justify-center gap-x-0 text-center whitespace-normal">
          <span className="mt-0.5 font-semibold leading-none group-data-[state=active]:text-foreground">
            {primaryLabel}&nbsp;
          </span>
          <span className="font-normal leading-none text-muted-foreground group-data-[state=active]:text-muted-foreground">
            {" "}
            · {secondaryLabel}
          </span>
        </span>
      </span>
    </TabsTrigger>
  );
}
