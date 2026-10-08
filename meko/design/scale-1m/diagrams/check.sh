#!/usr/bin/env bash
# check.sh <type> <spec.json>: archify showcase validation, diagnostics only
node "${ARCHIFY:-$HOME/.claude/skills/archify/bin/archify.mjs}" validate "$1" "$2" --quality showcase --json | python3 -c "
import sys,json; d=json.load(sys.stdin)
print('ok' if d.get('ok') else 'FAIL', {k:d[k] for k in ('artifactChecks','composition') if k in d})
for x in d.get('diagnostics',[])[:14]: print('-', x.get('code'), (x.get('subject') or {}).get('id',''), '|', str(x.get('message'))[:170])"
