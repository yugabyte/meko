// Sample PR diff for the ADLC Review Agent Lab's fixture ("in-built repo")
// path — used when the user hasn't pasted a GitHub PAT. Deliberately trips
// three real issues so the review has something concrete to flag: a bare
// `except:` that swallows every error, `team_id` concatenated straight into
// the SQL query (injection), and no validation on that same input.

export const SAMPLE_DIFF_TITLE = "Add per-team spend aggregation endpoint";

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

export function buildSampleDiffFile(): File {
  return new File([SAMPLE_DIFF_CONTENT], "spend-summary.diff", { type: "text/x-diff" });
}
