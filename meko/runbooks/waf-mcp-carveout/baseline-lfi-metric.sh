#!/bin/bash
# Daily Sum of the /mcp LFI count rule (LFIBodyOnMcp). Applied 2026-08-04.
AWS_PROFILE=AdministratorAccess-751821720815 aws cloudwatch get-metric-statistics \
  --region us-east-2 --namespace AWS/WAFV2 --metric-name CountedRequests \
  --dimensions Name=WebACL,Value=meko-prod-WAF Name=Region,Value=us-east-2 Name=Rule,Value=LFIBodyOnMcp \
  --start-time "$(date -u -v-10d '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || date -u -d '10 days ago' '+%Y-%m-%dT%H:%M:%SZ')" \
  --end-time "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" --period 86400 --statistics Sum
