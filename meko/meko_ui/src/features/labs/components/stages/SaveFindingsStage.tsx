// Closes the loop: the review's findings go into a datapack, so the next
// agent reads them instead of the thread being read once and lost.
//
// Writes into a datapack the user picks, and deliberately does not touch
// session.datapackId: the right-hand pane and every later stage still belong
// to the datapack built in Chapter 1.

import { useState } from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { WarningBanner } from "@/components/ui/warning-banner";
import {
  createDatapack,
  getApiErrorMessage,
  getDatapackProvisionStatus,
} from "@/lib/api/mekoApi";
import { addMemory, getInferenceGatewayErrorMessage } from "@/lib/inferenceGateway";

import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import { LabChoiceCard } from "../LabChoiceCard";
import type { PrReviewRunState } from "../demo-apps/PrReviewDemoApp";
import { LabStageHeader } from "./LabStageHeader";

/** Suggested name for the feature this lab's sample PR belongs to. Only a
 *  default: the user can rename it or write into a datapack they already
 *  have. */
const SUGGESTED_NAME = "feature/tenant-usage-report";

type Destination = "new" | "existing";

function findingLine(finding: { severity: string; location: string; comment: string }): string {
  const where = finding.location ? ` at ${finding.location}` : "";
  return `Review finding (${finding.severity})${where}: ${finding.comment}`;
}

export function SaveFindingsStage({
  session,
  run,
  onComplete,
}: {
  session: ReturnType<typeof useLabRunnerSession>;
  /** Chapter 3's run. Its findings are what gets written. */
  run: PrReviewRunState | null;
  onComplete: () => void;
}) {
  const [destination, setDestination] = useState<Destination>("new");
  const [newName, setNewName] = useState(SUGGESTED_NAME);
  const [existingId, setExistingId] = useState<string | null>(null);
  const [writing, setWriting] = useState(false);
  const [writtenTo, setWrittenTo] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const findings = Array.isArray(run?.reviewFindings) ? run.reviewFindings : [];
  const trimmedName = newName.trim();

  // A datapack of that name already exists, so "create" would reuse it. Worth
  // saying before the click, not after.
  const nameAlreadyExists = session.accessibleDatapacks.some(
    (dp) => dp.datapack_name.trim().toLowerCase() === trimmedName.toLowerCase(),
  );

  const canWrite =
    findings.length > 0 &&
    !writing &&
    writtenTo === null &&
    (destination === "new" ? trimmedName.length >= 3 : existingId !== null);

  const handleWrite = async () => {
    setWriting(true);
    setError(null);
    try {
      let targetId: string;
      let targetName: string;
      if (destination === "existing") {
        const chosen = session.accessibleDatapacks.find((dp) => dp.datapack_id === existingId);
        if (!chosen) throw new Error("Pick a datapack to write into.");
        targetId = chosen.datapack_id;
        targetName = chosen.datapack_name;
      } else {
        const existing = session.accessibleDatapacks.find(
          (dp) => dp.datapack_name.trim().toLowerCase() === trimmedName.toLowerCase(),
        );
        targetId =
          existing?.datapack_id ??
          (await createDatapack({ name: trimmedName, user_id: session.userId ?? "" })).datapack_id;
        targetName = trimmedName;
      }

      // Its own MCP key: the lab datapack's token doesn't grant access here.
      const provision = await getDatapackProvisionStatus(targetId);
      const token = provision.mcp_api_key;
      if (!token) throw new Error("That datapack isn't ready to write to yet. Try again in a moment.");

      for (const finding of findings) {
        await addMemory({ text: findingLine(finding), user_token: token, datapack_id: targetId });
      }

      setWrittenTo(targetName);
      onComplete();
      session.recordLessonTrace({
        lessonTitle: "Save the findings",
        calls: [
          {
            name: "datapack_create",
            durationMs: 0,
            status: destination === "existing" || nameAlreadyExists ? "skipped" : "ok",
          },
          { name: "memory_add", durationMs: 0, status: "ok", detail: `${findings.length} findings` },
        ],
      });
    } catch (err) {
      setError(
        getInferenceGatewayErrorMessage(err) ??
          getApiErrorMessage(err, "Couldn't write the findings to a datapack."),
      );
    } finally {
      setWriting(false);
    }
  };

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 4 · Step 1"
        title="Findings are context too"
        lede="A review that ends in a comment thread gets read once. Write it into a datapack and it becomes something the next agent reads before it writes a line."
        tags={["Datapacks"]}
      />

      {findings.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          Chapter 3's review didn't return structured findings, so there's nothing to write here. Continue when you're ready.
        </p>
      ) : (
        <>
          <div className="grid gap-3 sm:grid-cols-2">
            <LabChoiceCard
              title="A new datapack for this feature"
              body="Keeps the review separate from the standards it was checked against, so each can be shared on its own."
              selected={destination === "new"}
              onSelect={() => setDestination("new")}
            />
            <LabChoiceCard
              title="A datapack you already have"
              body="Write the findings alongside context you've already built, including the one this lab used."
              selected={destination === "existing"}
              onSelect={() => setDestination("existing")}
            />
          </div>

          {destination === "new" ? (
            <div className="flex flex-col gap-2">
              <Label htmlFor="findings-datapack-name">New datapack name</Label>
              <Input
                id="findings-datapack-name"
                className="font-mono"
                value={newName}
                onChange={(e) => setNewName(e.target.value)}
                spellCheck={false}
              />
              <p className="text-xs text-muted-foreground">
                {nameAlreadyExists
                  ? "You already have a datapack with this name, so the findings go into that one rather than a second copy."
                  : "This datapack doesn't exist yet. It's created when you write the findings."}
              </p>
            </div>
          ) : (
            <div className="flex flex-col gap-2">
              <Label htmlFor="findings-datapack-select">Write into</Label>
              <Select value={existingId ?? undefined} onValueChange={setExistingId}>
                <SelectTrigger id="findings-datapack-select" aria-label="Select datapack">
                  <SelectValue placeholder="Select a datapack" />
                </SelectTrigger>
                <SelectContent className="max-h-[300px]">
                  {session.accessibleDatapacks.map((dp) => (
                    <SelectItem key={dp.datapack_id} value={dp.datapack_id}>
                      {dp.datapack_name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          )}

          <div className="rounded-lg border border-border bg-card p-4 font-mono text-xs">
            <div className="text-foreground">
              {destination === "new" ? trimmedName || SUGGESTED_NAME : (session.accessibleDatapacks.find((dp) => dp.datapack_id === existingId)?.datapack_name ?? "…")}
            </div>
            <div className="mt-1 pl-4 text-muted-foreground">
              └─ review findings · {writtenTo ? `${findings.length} written by agentk` : `${findings.length} to write`}
            </div>
          </div>
        </>
      )}

      {error && <WarningBanner tone="warning">{error}</WarningBanner>}
      {writtenTo && (
        <WarningBanner tone="success">
          Written to <strong className="font-semibold">{writtenTo}</strong>. Anything you share that
          datapack with reads these before it starts.
        </WarningBanner>
      )}

      <div className="flex flex-wrap items-center gap-3">
        <Button onClick={() => void handleWrite()} disabled={!canWrite}>
          {writtenTo ? "Findings written ✓" : writing ? "Writing…" : "Write findings"}
        </Button>
      </div>
    </div>
  );
}
