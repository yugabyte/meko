// Recovers a review's findings from the agent's raw response when the agent's
// own parse of it failed. Pure; see recoverReviewFindings for why this exists
// on the client at all.

import type { ReviewFinding } from "../data/adlcReviewAgentLab/anthropicReferenceReview";

const VALID_SEVERITIES = ["critical", "major", "minor"] as const;

/** A `"` closes a string only when the next thing that matters is structural.
 *  Anything else means the model wrote a quote it should have escaped. */
const STRUCTURAL_AFTER_STRING = new Set([",", ":", "}", "]"]);

/** Re-escapes `"` characters left raw inside a string value. The reviews
 *  quote the code they flag and the code contains quotes:
 *
 *    "location": "... WHERE team_id = " + team_id + " GROUP BY user_id"
 *
 *  which ends the JSON string at `team_id = ` and makes the rest syntax
 *  garbage. The same model escapes correctly on the finding above it, so this
 *  is not something a prompt prevents. Structure is untouched, so input that
 *  was already valid passes through unchanged. */
function escapeStrayQuotes(text: string): string {
  let out = "";
  let inString = false;
  let escaped = false;
  for (let i = 0; i < text.length; i += 1) {
    const ch = text[i];
    if (!inString) {
      out += ch;
      if (ch === '"') inString = true;
      continue;
    }
    if (escaped) {
      out += ch;
      escaped = false;
      continue;
    }
    if (ch === "\\") {
      out += ch;
      escaped = true;
      continue;
    }
    if (ch === '"') {
      const rest = text.slice(i + 1).trimStart();
      if (rest.length === 0 || STRUCTURAL_AFTER_STRING.has(rest[0])) {
        out += ch;
        inString = false;
      } else {
        out += '\\"';
      }
      continue;
    }
    out += ch;
  }
  return out;
}

/** Strips the ```json fence the models add despite being told not to. */
function stripFence(text: string): string {
  const trimmed = text.trim();
  if (!trimmed.startsWith("```")) return trimmed;
  const unfenced = trimmed.replace(/^`+/, "").replace(/`+$/, "").trim();
  return unfenced.startsWith("json") ? unfenced.slice("json".length).trim() : unfenced;
}

function toFindings(value: unknown): ReviewFinding[] | null {
  if (typeof value !== "object" || value === null) return null;
  const raw = (value as { findings?: unknown }).findings;
  if (!Array.isArray(raw)) return null;
  const findings: ReviewFinding[] = [];
  for (const item of raw) {
    if (typeof item !== "object" || item === null) continue;
    const entry = item as Record<string, unknown>;
    const comment = String(entry.comment ?? "").trim();
    if (!comment) continue;
    const severity = String(entry.severity ?? "minor").trim().toLowerCase();
    findings.push({
      severity: (VALID_SEVERITIES as readonly string[]).includes(severity)
        ? (severity as ReviewFinding["severity"])
        : "minor",
      location: String(entry.location ?? "").trim(),
      comment,
    });
  }
  return findings;
}

/**
 * Findings read out of `reviewText`, or null when there are none to read.
 *
 * The agent parses its own response and sends the result as `findings`. When
 * that parse fails it sends `[]` and the raw response as `review` — and `[]`
 * is also what a genuinely clean diff returns, so the two are
 * indistinguishable on the wire. A real run on 2026-10-07 returned six
 * findings, five critical, and the lab reported "No blocking issues found"
 * because one `location` pasted the offending SQL with its quotes unescaped.
 *
 * So this re-reads the raw text the agent already sends us. Only ever used to
 * REPLACE an empty list: a non-empty result means the diff was not clean,
 * whatever the parse said. The real fix belongs in the agent, where every
 * other consumer of it would benefit too.
 */
export function recoverReviewFindings(reviewText: string | null): ReviewFinding[] | null {
  if (!reviewText?.trim()) return null;
  const body = stripFence(reviewText);
  for (const candidate of [body, escapeStrayQuotes(body)]) {
    try {
      const findings = toFindings(JSON.parse(candidate));
      if (findings && findings.length > 0) return findings;
    } catch {
      // Next candidate; a response that is genuinely prose yields nothing.
    }
  }
  return null;
}
