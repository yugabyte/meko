# WAF /mcp carve-out — prod baseline kit

Lets legitimate MCP capture writes reach the app while keeping WAF protection on every
other path. Two increments, both on meko-prod-WAF (acct 751821720815, us-east-2):

## 1. LFI / path-traversal (MEKO-410) — applied 2026-08-04
- CommonRuleSet override `GenericLFI_BODY` -> Count (pri 13)
- `BlockGenericLFIBodyExceptMCP` (pri 18, Block off `^/mcp`)
- `CountGenericLFIBodyOnMCP` (pri 19, Count, metric `LFIBodyOnMcp`)
- Anomaly alarm live: `meko-prod-WAF-LFIBodyOnMcp-anomaly` (SNS `meko-prod-waf-alerts`, no subscriber yet)
- Baseline over 9 days: ~44k hits/day (24k-73k), concentrated legit IPs.

## 2. Injection: XSS / SQLi / Log4J (MEKO-484; meko-skills#13) — applied 2026-08-13
- CommonRuleSet override adds `CrossSiteScripting_BODY` -> Count (pri 13)
- KnownBadInputs override adds `Log4JRCE_BODY` -> Count (pri 14)
- SQLiRuleSet override adds `SQLi_BODY` -> Count (pri 15)
- `BlockInjectionBodyExceptMCP` (pri 20, Block off `^/mcp`)
- `CountInjectionBodyOnMCP` (pri 21, Count, metric `InjectionBodyOnMcp`)
- Verified live: injection signatures on /mcp -> 401; still 403 on non-/mcp.
- Bake ~7-10 days (review ~2026-08-23 to 08-25), then add an anomaly alarm on
  `InjectionBodyOnMcp` mirroring the LFI one.

## Scripts
- baseline-lfi-metric.sh        - daily Sum of LFIBodyOnMcp
- baseline-injection-metric.sh  - daily Sum of InjectionBodyOnMcp

WAF logging goes to CloudWatch Logs group `aws-waf-logs-meko-prod` for per-path audit.

## Related client-side defects (capture.js, not WAF)
- MEKO-485 - opaque http_403 (real reason discarded)
- MEKO-486 - watermark held forever on persistent failure
