#!/usr/bin/env bash
#
# Claude Code PreToolUse hook: enforce the meko remote branch-naming
# convention on `git push`.
#
# Branches must be <alias>/<feature_name> or feature/<feature_name>, where
# <alias> is the author's short handle (hsunder, anijhawan, ...). An optional
# leading ticket/issue number in the feature part is allowed, e.g.
# hsunder/174-ingest-perf-hardening or feature/270-free-tier-quota.
#
# Conventional-commit-style type prefixes (fix/, feat/, chore/, docs/, ...) are
# NOT aliases and are rejected — `feature/` is the only non-alias prefix
# allowed. Matching is case-insensitive, so `Fix/` is rejected too.
# main/master are exempt; deletions and tag pushes are ignored.
#
# Reads the tool call as JSON on stdin. Blocks the Bash call (exit 2, reason on
# stderr) when a push would create/update a non-conforming branch. Fails OPEN:
# any parse ambiguity allows the push rather than risk a false block.

# Extract the command only for Bash tool calls; anything else -> allow.
cmd=$(jq -r 'if .tool_name=="Bash" then (.tool_input.command // "") else "" end' 2>/dev/null) || exit 0
[ -n "$cmd" ] || exit 0
case "$cmd" in *push*) ;; *) exit 0 ;; esac

exempt_re='^(main|master)$'
pattern_re='^[A-Za-z0-9][A-Za-z0-9._-]*/([0-9]+-)?[A-Za-z0-9][A-Za-z0-9._-]*$'
# Type prefixes that are not user aliases. `feature/` is deliberately absent —
# it is the one non-alias prefix meko allows. Tested against a lowercased ref,
# so `Fix/`, `FEAT/` and friends can't dodge it by capitalising.
badprefix_re='^(feat|fix|bug|bugfix|hotfix|chore|docs|doc|test|tests|refactor|perf|style|ci|build|release|wip|tmp|temp|dev)/'
assign_re='^[A-Za-z_][A-Za-z0-9_]*='
value_flags=' -o --push-option --receive-pack --exec --repo '

# --- tokenizer -------------------------------------------------------------
# Walk the command character by character, tracking quote state, and emit
# tokens into TOKS[]. Quotes are removed as we go, so a quoted-but-valid ref
# (`git push origin "hsunder/topic"`) is seen as the bare name; and separators
# are only honoured when unquoted, so a `;`/`&&` inside a commit message never
# fabricates a push statement. Two sentinels are emitted:
#   \x01  unquoted statement separator (&& || ; | & newline)
#   \x02  unquoted redirection (> >> < 2>&1 ...) — nothing after it is a refspec
# Returns non-zero on an unterminated quote, which the caller treats as
# ambiguous and allows.
SEP=$'\x01' RDR=$'\x02'
tokenize() {
  # NB: two statements — `local s=$1 n=${#s}` would expand ${#s} against the
  # caller's `s`, not this one, since all words expand before `local` runs.
  local s=$1
  local n=${#s} i=0 c q='' cur='' held=0
  TOKS=()
  while [ "$i" -lt "$n" ]; do
    c=${s:i:1}
    if [ "$q" = "'" ]; then
      if [ "$c" = "'" ]; then q=''; else cur+=$c; fi
    elif [ "$q" = '"' ]; then
      if [ "$c" = '"' ]; then
        q=''
      elif [ "$c" = $'\\' ] && [ $((i + 1)) -lt "$n" ]; then
        i=$((i + 1))
        cur+=${s:i:1}
      else cur+=$c; fi
    else
      case $c in
      "'" | '"')
        q=$c
        held=1
        ;;
      $'\\')
        i=$((i + 1))
        cur+=${s:i:1}
        held=1
        ;;
      ' ' | $'\t')
        if [ -n "$cur" ] || [ "$held" -eq 1 ]; then
          TOKS+=("$cur")
          cur=''
          held=0
        fi
        ;;
      ';' | '&' | '|' | $'\n')
        if [ -n "$cur" ] || [ "$held" -eq 1 ]; then
          TOKS+=("$cur")
          cur=''
          held=0
        fi
        TOKS+=("$SEP")
        # swallow a run of separator chars (&&, ||, ;;)
        while [ $((i + 1)) -lt "$n" ]; do
          case ${s:i+1:1} in ';' | '&' | '|' | $'\n') i=$((i + 1)) ;; *) break ;; esac
        done
        ;;
      '>' | '<')
        if [ -n "$cur" ] || [ "$held" -eq 1 ]; then
          # A bare fd number (the `2` in `2>&1`) belongs to the
          # redirection, not to the argument list.
          case $cur in '' | *[!0-9]*) TOKS+=("$cur") ;; esac
          cur=''
          held=0
        fi
        TOKS+=("$RDR")
        ;;
      *) cur+=$c ;;
      esac
    fi
    i=$((i + 1))
  done
  if [ -n "$cur" ] || [ "$held" -eq 1 ]; then TOKS+=("$cur"); fi
  [ -z "$q" ] # unterminated quote -> ambiguous
}

tokenize "$cmd" || exit 0

# --- per-statement analysis ------------------------------------------------
bad=''
stmt=()
flush() {
  local n=${#stmt[@]} i=0 j tok ref src dst deletion=0 skip=0 remote_seen=0 low
  [ "$n" -gt 0 ] || return 0

  # Command position: skip leading VAR=val assignments, then require the
  # command itself to be git (`git`, `/usr/bin/git`). Scanning for `git`
  # anywhere would misread `echo run git push origin fix/x` as a real push.
  while [ "$i" -lt "$n" ] && [[ ${stmt[$i]} =~ $assign_re ]]; do i=$((i + 1)); done
  [ "$i" -lt "$n" ] || return 0
  case "${stmt[$i]}" in git | */git) ;; *) return 0 ;; esac
  i=$((i + 1))

  # Skip git's global options (-C/-c take a value) to reach the subcommand.
  while [ "$i" -lt "$n" ]; do
    case "${stmt[$i]}" in
    -C | -c) i=$((i + 2)) ;;
    -*) i=$((i + 1)) ;;
    *) break ;;
    esac
  done
  # Only act when the subcommand is exactly `push` — `git stash push` etc. are
  # not our business.
  [ "$i" -lt "$n" ] && [ "${stmt[$i]}" = push ] || return 0
  i=$((i + 1))

  # Pre-scan for a deletion flag: git accepts `--delete` after the refspec
  # too, so encounter order can't be trusted.
  for ((j = i; j < n; j++)); do
    case "${stmt[$j]}" in
    "$RDR") break ;;
    --delete | -d)
      deletion=1
      break
      ;;
    esac
  done
  [ "$deletion" -eq 1 ] && return 0

  while [ "$i" -lt "$n" ]; do
    tok="${stmt[$i]}"
    i=$((i + 1))
    [ "$tok" = "$RDR" ] && break
    if [ "$skip" -eq 1 ]; then
      skip=0
      continue
    fi
    case "$value_flags" in *" $tok "*)
      skip=1
      continue
      ;;
    esac
    case "$tok" in -*) continue ;; esac
    if [ "$remote_seen" -eq 0 ]; then
      remote_seen=1
      continue
    fi # remote name
    ref=${tok#+}
    case "$ref" in
    *:*)
      src=${ref%%:*} dst=${ref#*:}
      { [ -z "$src" ] || [ -z "$dst" ]; } && continue # ':branch' == delete
      ref=$dst
      ;;
    esac
    # Tags are not branches — an explicit tag push is none of our business.
    case "$ref" in refs/tags/*) continue ;; esac
    ref=${ref#refs/heads/}
    [ -n "$ref" ] || continue
    # `git push -u origin HEAD` is what a first push looks like, and it
    # names the current branch — resolve it so branch creation, the case
    # this hook exists for, isn't trivially bypassed. Detached HEAD or a
    # failed lookup falls open.
    if [ "$ref" = HEAD ]; then
      ref=$(git branch --show-current 2>/dev/null)
      [ -n "$ref" ] || continue
    fi
    [[ $ref =~ $exempt_re ]] && continue
    # A bare name that resolves to an existing tag is a tag push.
    git show-ref --tags --verify --quiet "refs/tags/$ref" 2>/dev/null && continue
    low=${ref,,}
    if [[ $ref =~ $pattern_re ]] && ! [[ $low =~ $badprefix_re ]]; then
      continue
    fi
    bad="$bad $ref"
  done
}

for tok in "${TOKS[@]}"; do
  if [ "$tok" = "$SEP" ]; then
    flush
    stmt=()
  else
    stmt+=("$tok")
  fi
done
flush

if [ -n "$bad" ]; then
  echo "Blocked by branch-naming hook:$bad must match <alias>/<feature_name>" \
    "or feature/<feature_name>, with an optional leading ticket number —" \
    "e.g. hsunder/174-ingest-perf-hardening, feature/artifact-upload." \
    "Type prefixes (fix/, feat/, chore/, docs/, ...) are not aliases." \
    "Rename the branch before pushing. See AGENTS.md > Branch naming." >&2
  exit 2
fi
exit 0
