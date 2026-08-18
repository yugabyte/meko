#!/bin/bash
# Daily Sum of the /mcp injection (XSS/SQLi/Log4J) count rule (InjectionBodyOnMcp).
# Applied to prod 2026-08-13; review ~2026-08-23 to 08-25.
AWS_PROFILE=AdministratorAccess-751821720815 aws cloudwatch get-metric-statistics \
  --region us-east-2 --namespace AWS/WAFV2 --metric-name CountedRequests \
  --dimensions Name=WebACL,Value=meko-prod-WAF Name=Region,Value=us-east-2 Name=Rule,Value=InjectionBodyOnMcp \
  --start-time "$(date -u -v-10d '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || date -u -d '10 days ago' '+%Y-%m-%dT%H:%M:%SZ')" \
  --end-time "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" --period 86400 --statistics Sum
