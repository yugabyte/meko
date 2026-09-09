// PLACEHOLDER, not yet a real capture -- every value below is 0/empty until
// someone runs `canned-agents/adlc-review-agent/scripts/gen_reference_review.py`
// (needs AWS credentials with bedrock:InvokeModelWithResponseStream, which the
// account's meko_ybm_admin identity does not have, and a real datapack whose
// knowledge base already has org-coding-standards.md uploaded) and pastes its
// output in here. Once populated, this becomes a real, one-time-captured Claude
// review of SAMPLE_DIFF_CONTENT (sampleDiff.ts), grounded in
// ORG_CODING_STANDARDS_CONTENT (orgCodingStandards.ts) the same way review.py's
// run_review_stream builds its prompt -- the real run_review() production path
// (a real knowledgebase_search/memory_search against a real datapack, a real
// AnthropicBedrock call, and the real context_stats datapack_describe reports),
// not fabricated. Shown as a static reference in "Run a Review" so the token
// cost of a small model (chosen from the Model dropdown) has something concrete
// to compare against, independent of whether a deployed AgentK instance is
// currently reachable.
//
// ANTHROPIC_REFERENCE_IS_POPULATED gates both AnthropicReferenceCard (skips
// rendering the reference card at all while this is false, rather than showing
// "No issues found." as if that were a real result) and the cost-savings guard
// in PrReviewDemoApp -- update it here if the populated shape ever changes.

export type ReviewFinding = { severity: "critical" | "major" | "minor"; location: string; comment: string };

export const ANTHROPIC_REFERENCE_MODEL_LABEL = "Claude Sonnet 4.5";
export const ANTHROPIC_REFERENCE_MODEL_ID = "anthropic.claude-sonnet-4-5-20250929-v1:0";

// review.py's REVIEW_SYSTEM_PROMPT asks for structured findings JSON, not
// free prose -- ANTHROPIC_REFERENCE_FINDINGS is the primary content
// (rendered via FindingsList); ANTHROPIC_REFERENCE_REVIEW_TEXT is
// ReviewResult.text, the markdown-rendered fallback of the same findings
// (kept for parity with what the real run actually returns, not shown
// directly in the UI).
export const ANTHROPIC_REFERENCE_FINDINGS: ReviewFinding[] = [];
export const ANTHROPIC_REFERENCE_REVIEW_TEXT = "PENDING_REAL_GENERATION";

export const ANTHROPIC_REFERENCE_INPUT_TOKENS = 0;
export const ANTHROPIC_REFERENCE_OUTPUT_TOKENS = 0;

// A real capture always has non-zero usage -- gen_reference_review.py's own
// run_review() call fails outright on an empty response, so this is a safe,
// single source of truth for "has anyone run the capture yet."
export const ANTHROPIC_REFERENCE_IS_POPULATED = ANTHROPIC_REFERENCE_INPUT_TOKENS > 0;

// Mirrors review.py's ReviewStreamEvent context_stats shape exactly (see
// deploymentInvokeApi.ts) -- kb_chunks_total/memories_total come from a real
// datapack_describe call, kb_tokens_estimated/memory_tokens_estimated are each
// blob's share of the real ANTHROPIC_REFERENCE_INPUT_TOKENS above, proportional
// to its share of the system prompt's character count (Bedrock exposes no
// pre-call tokenizer for AnthropicBedrock, so this is an estimate, not an exact
// per-section count -- see review.py's _build_context_stats for the full
// rationale).
export const ANTHROPIC_REFERENCE_CONTEXT_STATS = {
  kbChunksRetrieved: 0,
  kbChunksTotal: null as number | null,
  kbTokensEstimated: 0,
  memoriesRetrieved: 0,
  memoriesTotal: null as number | null,
  memoryTokensEstimated: 0,
};
