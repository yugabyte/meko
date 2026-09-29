// Sample PR diff for the ADLC Review Agent Lab's fixture ("in-built repo")
// path — used when the user hasn't pasted a GitHub PAT. Deliberately trips
// three real issues so the review has something concrete to flag: a bare
// `except:` that swallows every error, `team_id` concatenated straight into
// the SQL query (injection), and no validation on that same input.

export const SAMPLE_DIFF_TITLE = "Add per-team spend aggregation endpoint";

// A short, PR-author-style description -- shown above the diff the same way
// a real GitHub PR shows its own description, so "what's being reviewed"
// reads like a real PR, not just a bare code diff.
export const SAMPLE_DIFF_DESCRIPTION =
  "Adds a GET /spend/summary endpoint so the billing dashboard can show token " +
  "spend per team. Put together quickly to unblock this week's usage report -- " +
  "flagging for a closer look at error handling and the raw SQL before merge.";

export const SAMPLE_DIFF_CONTENT = `diff --git a/api/spend.py b/api/spend.py
index 1a2b3c4..5d6e7f8 100644
--- a/api/spend.py
+++ b/api/spend.py
@@ -10,6 +10,24 @@ from flask import Blueprint, request, jsonify
 spend_bp = Blueprint("spend", __name__)


+@spend_bp.route("/spend/summary", methods=["GET"])
+def spend_summary():
+    team_id = request.args.get("team_id")
+    try:
+        rows = db.execute(
+            "SELECT user_id, SUM(tokens) FROM spend WHERE team_id = "
+            + team_id
+            + " GROUP BY user_id"
+        )
+        total = sum(r[1] for r in rows)
+        return jsonify({"team_id": team_id, "total_tokens": total, "rows": rows})
+    except:
+        return jsonify({"error": "failed"}), 500
+
+
 @spend_bp.route("/spend/user/<user_id>", methods=["GET"])
 def spend_for_user(user_id):
     rows = db.execute(
`;

/** 1-based line number, in the rendered diff, of the line that concatenates
 *  request input straight into the SQL string. Chapter 1's whole argument
 *  points at this line, so it is derived rather than written down — editing
 *  the diff above moves the banner with it instead of leaving it lying. */
export const SAMPLE_DIFF_SQL_CONCAT_LINE =
  SAMPLE_DIFF_CONTENT.split("\n").findIndex((line) => line.includes("+ team_id")) + 1;

/** Lines of Python this PR adds, for copy that quotes the size. Same reason:
 *  the header's own +/- bar counts them, so prose must not disagree. */
export const SAMPLE_DIFF_ADDED_LINES = SAMPLE_DIFF_CONTENT.split("\n").filter(
  (line) => line.startsWith("+") && !line.startsWith("+++"),
).length;

export function buildSampleDiffFile(): File {
  return new File([SAMPLE_DIFF_CONTENT], "spend-summary.diff", { type: "text/x-diff" });
}
