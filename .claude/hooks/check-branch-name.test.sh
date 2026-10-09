#!/usr/bin/env bash
#
# Tests for check-branch-name.sh. Run from anywhere:
#   bash .claude/hooks/check-branch-name.test.sh
#
# Each case feeds a PreToolUse payload on stdin and asserts allow (exit 0) or
# block (exit 2). Cases marked FIXED-* are regressions for review findings on
# PR #387 — they all failed before the tokenizer rewrite.

cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1
HOOK=.claude/hooks/check-branch-name.sh
pass=0 fail=0

check() { # check <expected: allow|block> <command>
  local want=$1 cmd=$2 got rc
  printf '%s' "$cmd" | jq -Rs '{tool_name:"Bash",tool_input:{command:.}}' |
    bash "$HOOK" >/dev/null 2>&1
  rc=$?
  [ "$rc" -eq 0 ] && got=allow || got=block
  if [ "$got" = "$want" ]; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1))
    printf 'FAIL  want=%-5s got=%-5s  %s\n' "$want" "$got" "$cmd"
  fi
}

# --- conforming branch names -> allow --------------------------------------
check allow 'git push -u origin hsunder/174-ingest-perf-hardening'
check allow 'git push origin feature/artifact-upload'
check allow 'git push origin anijhawan/app-no-cognito'
check allow 'git push origin feature/270-free-tier-quota'
check allow 'git push'
check allow 'git push origin main'
check allow 'git push origin master'
check allow 'git push -o ci.skip origin hsunder/topic'
check allow 'git push --force-with-lease origin hsunder/topic'
check allow 'git push origin HEAD:refs/heads/hsunder/topic'
check allow '/usr/bin/git push origin hsunder/topic'
check allow 'GIT_TRACE=1 git push origin hsunder/topic'

# --- non-conforming -> block ------------------------------------------------
check block 'git push origin change-qs'
check block 'git push origin MEKO-313-allow-mem-extraction'
check block 'git push origin fix/cross-account-scoping'
check block 'git push origin feat/MEKO-270-free-tier-quota'
check block 'git push origin docs/quick-start-serial-flow'
check block 'git push origin bug/meko-311-fix'
check block 'git push origin chore/auth-footer'
check block 'git push origin HEAD:chore/auth-footer'
check block 'git push origin hsunder/ok fix/bad'
check block 'git push origin +fix/bad'

# --- not a push at all -> allow --------------------------------------------
check allow 'git stash push -m wip'
check allow 'git commit -m "about to push feat/x"'
check allow 'echo pushing'
check allow 'git log --oneline | grep push'

# --- deletions and tags are ignored ----------------------------------------
check allow 'git push origin --delete fix/old-branch'
check allow 'git push origin :fix/old-branch'
# FIXED-1: --delete after the refspec is valid git; encounter order was trusted.
check allow 'git push origin fix/old-branch --delete'
check allow 'git push origin fix/old-branch -d'
# FIXED-2: explicit tag pushes were matched against the branch pattern.
check allow 'git push origin refs/tags/v1.2.3'
check allow 'git push --tags origin'

# --- FIXED-3: denylist was case-sensitive ----------------------------------
check block 'git push origin Fix/bug-123'
check block 'git push origin FEAT/thing'
check block 'git push origin Docs/quick-start'

# --- FIXED-4: `git` was matched anywhere, not in command position ----------
check allow 'echo run git push origin fix/my-branch'
check allow 'printf "%s" "git push origin fix/x"'

# --- FIXED-5: quotes survived tokenization ---------------------------------
check allow 'git push origin "hsunder/174-ingest-perf-hardening"'
check allow "git push origin 'feature/artifact-upload'"
check block 'git push origin "fix/bad-branch"'
# A separator inside a quoted string must not fabricate a push statement.
check allow 'git commit -m "part1 && git push origin fix/bad-branch"'
check allow 'git commit -m "wip; git push origin fix/bad"'
# ...but a real separator still splits.
check block 'git add -A && git push origin fix/bad'
check block 'true; git push origin fix/bad'

# --- ambiguity fails open ---------------------------------------------------
check allow 'git push origin "unterminated'

# --- redirection ends the refspec list -------------------------------------
check allow 'git push origin hsunder/topic 2>&1'
check block 'git push origin fix/bad 2>&1'

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
