"""Pin the response-shape contract for `_format_memory_context` and
`_format_knowledge_context`.

Both formatters are key-name-sensitive — twice now we've shipped silent
bugs where MCP returned data correctly but the formatter looked under the
wrong key (`text`/`content`/`snippet` instead of `chunk_text` for KB; and
ignoring the `relations` / `promoted_relations` arrays for memory). The
chat answer then degrades to "no grounding," with no log line to
diagnose.

These tests:

  - Lock the **real Meko MCP payload shapes** captured directly from the
    `knowledgebase_search` and `memory_search` tools, so any future
    server-side rename will fail loudly here instead of silently in
    production.
  - Cover the older / alternate shapes that legacy MCP servers emit
    (`matches[].snippet`, bare lists, `subject`/`predicate`/`object`
    triples) so the fallback chains stay alive.
  - Cover the malformed-triple guard so a half-baked relation never
    sneaks into the LLM's context.

These are pure-function unit tests — no async, no mocks, no LLM. They run
in milliseconds.
"""

from __future__ import annotations

from inference_gateway.chain import (
    MEMORY_MIN_SCORE,
    _format_knowledge_context,
    _format_memory_context,
    _is_trivial_query,
)


# --------------------------------------------------------------------------- #
# knowledgebase_search formatter
# --------------------------------------------------------------------------- #


def test_kb_real_meko_payload_extracts_chunk_text_and_attributes_document_id() -> None:
    """The exact shape Meko's `knowledgebase_search` returns today.

    Captured live from `mcp__meko__knowledgebase_search` — every key here
    is what the server actually emits. If this test breaks, the server
    changed a field name and the gateway needs to track it.
    """
    payload = {
        "results": [
            {
                "id": "b010401d-35d9-41d8-9cc5-24b2740031f9",
                "chunk_text": "I am John Doe.\nI like Soccer and Cricket",
                "document_id": "4d6038ec-c46a-41f8-8033-d343cc0944ca",
                "metadata_filters": {},
                "distance": 1.0166586768224495,
            },
            {
                "id": "3d07fcb9-7fdc-4f55-8fcd-3cebaaf186a0",
                "chunk_text": "Manchester United and Indian cricket team",
                "document_id": "2a01bbad-a67b-4926-bd01-d0a6b947cd2c",
                "metadata_filters": {},
                "distance": 1.0166586768224495,
            },
        ],
        "count": 2,
    }
    rendered = _format_knowledge_context(payload)
    # Both chunks present.
    assert "I am John Doe." in rendered
    assert "Manchester United and Indian cricket team" in rendered
    # Each bullet attributed by its document_id (no filename available
    # from the server today, so we fall back to the UUID).
    assert "[4d6038ec-c46a-41f8-8033-d343cc0944ca]" in rendered
    assert "[2a01bbad-a67b-4926-bd01-d0a6b947cd2c]" in rendered
    # Two bullets, separated by newline.
    assert rendered.count("\n- [") == 1  # one separator between two `- [...] ...` bullets


def test_kb_empty_results_returns_empty_string() -> None:
    """No hits → empty string, which downstream renders as `(none yet)`."""
    assert _format_knowledge_context({"results": [], "count": 0}) == ""
    assert _format_knowledge_context({}) == ""


def test_kb_legacy_shape_with_text_field_still_works() -> None:
    """Older / alternate MCP servers may surface text under `text` instead
    of `chunk_text`, and use `document_name` instead of `document_id`.
    The fallback chain must still produce a usable bullet."""
    payload = {
        "matches": [
            {"text": "old shape chunk", "document_name": "europe.md"},
        ],
    }
    rendered = _format_knowledge_context(payload)
    assert rendered == "- [europe.md] old shape chunk"


def test_kb_bare_list_payload() -> None:
    """Some MCP adapters drop the dict envelope and return a bare list."""
    payload = [{"chunk_text": "bare-list chunk"}]
    rendered = _format_knowledge_context(payload)
    assert rendered == "- bare-list chunk"


def test_kb_bullet_without_attribution_when_no_source_field() -> None:
    """If neither document_id nor any source key is present, the bullet
    just carries the text — better than dropping the hit entirely."""
    payload = {"results": [{"chunk_text": "anonymous chunk"}]}
    rendered = _format_knowledge_context(payload)
    assert rendered == "- anonymous chunk"


def test_kb_long_source_path_trimmed_to_basename() -> None:
    """Slash-prefixed sources (e.g. S3 keys) get trimmed so the bullet
    stays readable in the LLM prompt."""
    payload = {
        "results": [
            {"chunk_text": "x", "source": "s3://bucket/path/to/john.txt"},
        ],
    }
    rendered = _format_knowledge_context(payload)
    assert rendered == "- [john.txt] x"


# --------------------------------------------------------------------------- #
# memory_search formatter
# --------------------------------------------------------------------------- #


def test_memory_real_meko_payload_surfaces_results_and_relations() -> None:
    """The exact shape Meko's `memory_search` returns today.

    Captured live from `mcp__meko__memory_search` after seeding three
    memories about John. Three vector hits plus one graph relation must
    all reach the rendered context, each with its provenance tag.
    """
    payload = {
        "results": [
            {"id": "1", "memory": "Name is John",                  "score": 0.342},
            {"id": "2", "memory": "Likes basketball and soccer",  "score": 0.734},
            {"id": "3", "memory": "Would love to coach kids",     "score": 0.834},
        ],
        "relations": [
            {"source": "user", "relationship": "CONNECTED_TO", "destination": "john"},
        ],
        "promoted_relations": [],
    }
    rendered = _format_memory_context(payload)
    # All three vector hits carry the [mem] tag.
    assert "- [mem] Name is John" in rendered
    assert "- [mem] Likes basketball and soccer" in rendered
    assert "- [mem] Would love to coach kids" in rendered
    # Graph relation flattens to subject ─predicate→ object with [graph] tag.
    assert "- [graph] user CONNECTED_TO john" in rendered
    # Order matters: results first, then relations, then promoted.
    assert rendered.index("[mem]") < rendered.index("[graph]")


def test_memory_promoted_relations_get_distinct_provenance_tag() -> None:
    """`promoted_relations` is a separate Mem0 array — same shape as
    `relations`, but elevated to the tenant KB. The formatter tags them
    [promoted] so the LLM (and a debugger) can tell them apart."""
    payload = {
        "results": [],
        "relations": [],
        "promoted_relations": [
            {"source": "john", "relationship": "OWNS", "destination": "datapack"},
        ],
    }
    rendered = _format_memory_context(payload)
    assert rendered == "- [promoted] john OWNS datapack"


def test_memory_empty_payload_returns_empty_string() -> None:
    """No memories AND no relations → empty string (renders as `(none yet)`)."""
    assert _format_memory_context({"results": [], "relations": [], "promoted_relations": []}) == ""
    assert _format_memory_context({}) == ""


def test_memory_relations_only_no_vector_hits() -> None:
    """When the vector search misses but the graph has data, the LLM
    should still see the graph hits — important for entity-style queries
    that semantic similarity alone can't answer."""
    payload = {
        "results": [],
        "relations": [
            {"source": "john", "relationship": "LIKES", "destination": "soccer"},
        ],
    }
    assert _format_memory_context(payload) == "- [graph] john LIKES soccer"


def test_memory_bare_list_payload_treated_as_vector_hits() -> None:
    """Some adapters return just a list — handle it as the vector array."""
    rendered = _format_memory_context([{"memory": "bare list mem"}])
    assert rendered == "- [mem] bare list mem"


def test_memory_relation_alt_key_names_tolerated() -> None:
    """Older Mem0 / Apache AGE versions used `subject`/`predicate`/`object`.
    We accept those as fallbacks so a server upgrade doesn't silently
    drop graph data on the floor."""
    payload = {
        "results": [],
        "relations": [
            {"subject": "John", "predicate": "LIKES", "object": "soccer"},
        ],
    }
    assert _format_memory_context(payload) == "- [graph] John LIKES soccer"


def test_memory_malformed_triple_dropped_not_rendered_partially() -> None:
    """A triple missing any of source/relation/target is dropped. We'd
    rather show nothing than feed a half-fact like "john  soccer" to the
    LLM (which it could then hallucinate a predicate around)."""
    payload = {
        "results": [],
        "relations": [
            {"source": "john", "destination": "soccer"},          # no relation field
            {"relationship": "LIKES", "destination": "soccer"},  # no source
            {"source": "john", "relationship": "LIKES"},           # no destination
        ],
    }
    assert _format_memory_context(payload) == ""


def test_memory_legacy_shape_with_data_field() -> None:
    """If an MCP server uses `data` instead of `memory` for the text,
    keep extracting via the documented fallback chain."""
    payload = {"results": [{"data": "legacy text"}]}
    assert _format_memory_context(payload) == "- [mem] legacy text"


# --------------------------------------------------------------------------- #
# Relevance thresholds — Fix A: drop weak hits in the formatters so a vague
# query like "hi" doesn't smear five unrelated facts into the system prompt.
# --------------------------------------------------------------------------- #


def test_memory_threshold_drops_weak_scores() -> None:
    """Memories with `score < MEMORY_MIN_SCORE` are dropped before rendering.

    Anchors the contract: future loosening / tightening of the threshold
    requires a deliberate change here, not silent drift in production.
    """
    assert MEMORY_MIN_SCORE == 0.3  # update both if the default changes
    payload = {
        "results": [
            {"memory": "Strong hit",  "score": 0.85},
            {"memory": "Marginal hit", "score": 0.31},  # just above threshold
            {"memory": "Weak noise",   "score": 0.10},
        ],
    }
    rendered = _format_memory_context(payload)
    assert "Strong hit" in rendered
    assert "Marginal hit" in rendered
    assert "Weak noise" not in rendered


def test_memory_threshold_keeps_items_without_score() -> None:
    """Legacy / alt MCP servers may not return a `score` field. We surface
    those rather than silently dropping them — the threshold is a quality
    floor, not a "score required" gate."""
    payload = {"results": [{"memory": "No-score hit"}]}
    assert _format_memory_context(payload) == "- [mem] No-score hit"


def test_memory_threshold_does_not_affect_relations() -> None:
    """Graph relations are not scored — they should always pass through."""
    payload = {
        "results": [{"memory": "Weak", "score": 0.05}],  # dropped
        "relations": [{"source": "john", "relationship": "LIKES", "destination": "soccer"}],
    }
    rendered = _format_memory_context(payload)
    assert "Weak" not in rendered
    assert "john LIKES soccer" in rendered


def test_kb_renders_all_chunks_regardless_of_distance() -> None:
    """KB relevance is owned by the MCP's hybrid search — the gateway no longer
    filters by distance and renders every chunk it's given, even high-distance ones."""
    payload = {
        "results": [
            {"chunk_text": "Strong hit",   "distance": 0.4},
            {"chunk_text": "Marginal hit", "distance": 1.05},
            {"chunk_text": "Far chunk",    "distance": 1.5},
        ],
    }
    rendered = _format_knowledge_context(payload)
    assert "Strong hit" in rendered
    assert "Marginal hit" in rendered
    assert "Far chunk" in rendered


def test_kb_threshold_keeps_items_without_distance() -> None:
    """Older shapes may not include a distance — keep them rather than drop."""
    payload = {"results": [{"chunk_text": "No-distance hit"}]}
    assert "No-distance hit" in _format_knowledge_context(payload)


# --------------------------------------------------------------------------- #
# Trivial-query short-circuit — Fix B: skip retrieval entirely for greetings
# and very short inputs since their embeddings are too vague to retrieve on.
# --------------------------------------------------------------------------- #


def test_trivial_query_greetings_are_detected() -> None:
    """Common greetings / acknowledgements short-circuit retrieval."""
    for msg in ("hi", "Hello", "hey!", "thanks.", "thank you", "ok", "Bye", "yes", "nope"):
        assert _is_trivial_query(msg), f"expected trivial: {msg!r}"


def test_trivial_query_very_short_inputs_are_detected() -> None:
    """Anything below the min-length floor (4 chars) is trivial — even if
    it isn't a known greeting. Embeds too weakly to retrieve on."""
    for msg in ("k", "y", "ok", "yo"):
        assert _is_trivial_query(msg), f"expected trivial (too short): {msg!r}"


def test_trivial_query_real_questions_pass_through() -> None:
    """Genuine prompts must NOT be short-circuited."""
    for msg in (
        "what does John like",
        "Recommend a vacation",
        "Tell me about Spain",
        "hi, can you help me plan a trip",  # starts with greeting but is a real question
    ):
        assert not _is_trivial_query(msg), f"expected non-trivial: {msg!r}"


def test_trivial_query_whitespace_only_is_trivial() -> None:
    """Whitespace strings are too short — also trivial."""
    assert _is_trivial_query("")
    assert _is_trivial_query("   ")
    assert _is_trivial_query("\n\t")


# --------------------------------------------------------------------------- #
# Sort + dedup contracts. Mem0 doesn't guarantee a sort order on its side,
# and the KB indexer empirically emits duplicate chunks we need to filter.
# --------------------------------------------------------------------------- #


def test_memory_hits_sorted_by_score_desc() -> None:
    """Mem0 doesn't guarantee an order — we sort mem hits by score
    descending so the strongest match leads the list (and the prompt)."""
    payload = {
        "results": [
            {"memory": "Mid-strength",  "score": 0.50},
            {"memory": "Strongest",      "score": 0.90},
            {"memory": "Just above bar", "score": 0.35},
        ],
    }
    rendered = _format_memory_context(payload)
    # All three should be present (all above the 0.3 threshold).
    assert "Strongest" in rendered
    assert "Mid-strength" in rendered
    assert "Just above bar" in rendered
    # And in score-DESC order in the bulleted output.
    assert rendered.index("Strongest") < rendered.index("Mid-strength")
    assert rendered.index("Mid-strength") < rendered.index("Just above bar")


def test_memory_hits_keep_mem_then_graph_then_promoted_order() -> None:
    """Sorting by score is within the `mem` kind only — kind grouping
    (mem → graph → promoted) is preserved so callers don't have to
    re-bucket them."""
    payload = {
        "results": [{"memory": "vector hit", "score": 0.5}],
        "relations": [
            {"source": "user", "relationship": "LIKES", "destination": "soccer"},
        ],
        "promoted_relations": [
            {"source": "raj", "relationship": "OWNS", "destination": "datapack"},
        ],
    }
    rendered = _format_memory_context(payload)
    assert rendered.index("[mem]") < rendered.index("[graph]")
    assert rendered.index("[graph]") < rendered.index("[promoted]")


def test_kb_hits_sorted_by_distance_asc() -> None:
    """Closest match (lowest distance) leads the list. Explicit sort
    because the indexer isn't contractually obligated to return ordered
    results, even if it usually does."""
    payload = {
        "results": [
            {"chunk_text": "Far",     "distance": 1.10},
            {"chunk_text": "Closest", "distance": 0.40},
            {"chunk_text": "Middle",  "distance": 0.80},
        ],
    }
    rendered = _format_knowledge_context(payload)
    assert rendered.index("Closest") < rendered.index("Middle")
    assert rendered.index("Middle") < rendered.index("Far")


def test_kb_hits_preserve_indexer_duplicates() -> None:
    """We intentionally do NOT dedupe duplicate chunk_text. If the
    indexer surfaces the same chunk multiple times we render it as-is
    — the agent reasoning panel and the LLM prompt should be an honest
    view of what MCP returned. If duplicates become a UX problem the
    fix belongs upstream in the indexer, not here."""
    payload = {
        "results": [
            {"chunk_text": "duplicate body", "distance": 0.94, "document_id": "doc-a"},
            {"chunk_text": "duplicate body", "distance": 0.94, "document_id": "doc-a"},
            {"chunk_text": "duplicate body", "distance": 0.94, "document_id": "doc-a"},
        ],
    }
    rendered = _format_knowledge_context(payload)
    assert rendered.count("duplicate body") == 3
