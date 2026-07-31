#!/usr/bin/env bash
#
# Claude Code PreToolUse hook: enforce the yb-amp remote branch-naming
# convention on `git push`.
#
# Branches must be <alias>/<feature_name> (an optional leading GitHub issue
# number in the feature part is allowed, e.g. hsunder/174-ingest-perf-hardening).
# main/master are exempt; deletions and tag pushes are ignored.
#
# Reads the tool call as JSON on stdin. Blocks the Bash call (exit 2, reason on
# stderr) when a push would create/update a non-conforming branch. Fails OPEN:
# any parse ambiguity allows the push rather than risk a false block.

# Extract the command only for Bash tool calls; anything else -> allow.
cmd=$(jq -r 'if .tool_name=="Bash" then (.tool_input.command // "") else "" end' 2>/dev/null) || exit 0
[ -n "$cmd" ] || exit 0
case "$cmd" in *push*) ;; *) exit 0 ;; esac

exempt_re='^(main|master|HEAD)$'
pattern_re='^[A-Za-z0-9][A-Za-z0-9._-]*/([0-9]+-)?[A-Za-z0-9][A-Za-z0-9._-]*$'
value_flags=' -o --push-option --receive-pack --exec --repo '

# Split into simple statements on && || ; and pipes (pure bash, no
# subprocess). `||` is replaced before the single `|`.
stmts=${cmd//&&/$'\n'}
stmts=${stmts//||/$'\n'}
stmts=${stmts//;/$'\n'}
stmts=${stmts//|/$'\n'}

set -f  # no globbing while we word-split statements
bad=''
while IFS= read -r stmt; do
    case "$stmt" in *push*) ;; *) continue ;; esac
    # shellcheck disable=SC2206
    tokens=($stmt)
    n=${#tokens[@]}

    # Find `git`, then its subcommand (skipping global options, incl. the
    # value-taking -C/-c). Only act when the subcommand is exactly `push` —
    # so `git stash push`, `git commit -m "...push..."`, etc. are ignored.
    i=0
    while [ "$i" -lt "$n" ] && [ "${tokens[$i]}" != git ]; do i=$((i+1)); done
    i=$((i+1))
    while [ "$i" -lt "$n" ]; do
        case "${tokens[$i]}" in
            -C|-c) i=$((i+2)) ;;
            -*)    i=$((i+1)) ;;
            *)     break ;;
        esac
    done
    [ "$i" -lt "$n" ] && [ "${tokens[$i]}" = push ] || continue
    i=$((i+1))

    deletion=0 skip=0 remote_seen=0
    while [ "$i" -lt "$n" ]; do
        tok="${tokens[$i]}"; i=$((i+1))
        # Stop at a shell redirection/background token (e.g. 2>&1, >, >>, <,
        # &) — everything after it is not a refspec.
        case "$tok" in *'>'*|*'<'*|'&') break ;; esac
        if [ "$skip" -eq 1 ]; then skip=0; continue; fi
        case "$tok" in --delete|-d) deletion=1; continue ;; esac
        case "$value_flags" in *" $tok "*) skip=1; continue ;; esac
        case "$tok" in -*) continue ;; esac
        if [ "$remote_seen" -eq 0 ]; then remote_seen=1; continue; fi  # remote name
        [ "$deletion" -eq 1 ] && continue
        ref=${tok#+}
        case "$ref" in
            *:*)
                src=${ref%%:*} dst=${ref#*:}
                { [ -z "$src" ] || [ -z "$dst" ]; } && continue  # ':branch' == delete
                ref=$dst ;;
        esac
        ref=${ref#refs/heads/}
        [ -n "$ref" ] || continue
        [[ $ref =~ $exempt_re ]] && continue
        [[ $ref =~ $pattern_re ]] || bad="$bad $ref"
    done
done <<< "$stmts"
set +f

if [ -n "$bad" ]; then
    echo "Blocked by branch-naming hook:$bad must match <alias>/<feature_name>" \
         "(optional leading issue number, e.g. hsunder/174-ingest-perf-hardening)." \
         "Rename the branch before pushing. See AGENTS.md > Project conventions." >&2
    exit 2
fi
exit 0
