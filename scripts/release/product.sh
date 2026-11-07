# Sourced by the release scripts in this folder. The repo releases three products from one
# history, each with its own tags, versions and notes:
#
#   agentk  AgentK (src/extensions/agentk): tags agentk-v<version>, and v<version> before
#           releases were tagged by product (installed AgentKs from before read only those, so
#           the newest v… release, which reads both, stays: prune-releases.sh never deletes it)
#   meko    Meko (src/meko): tags meko-v<version>
#   present
#           Present (src/extensions/present): tags present-v<version>, whose tarballs Meko
#           installs and updates (meko extension install, meko extension update)
#
# Takes `--product agentk|meko|present` (default agentk) out of the script's arguments and sets:
#
#   product       agentk, meko or present
#   tag_prefix    the prefix of the product's new tags: tag = <tag_prefix><version>
#   tag_prefixes  every prefix the product's tags have used, newest scheme first
#   scope         an extended regex for the `(<component>[/<area>])` of the subjects that may
#                 release the product, and
#   exclude       one for those that don't, even though scope matches them. Meko takes the
#                 subjects that name meko, alone or in a list (feat(agentk,meko): ...), and
#                 Present those that name present. AgentK takes every subject but those that
#                 name only meko or present (or presenter, Present's component before it was
#                 present): agentk, repo, lists naming agentk or repo, and old subjects with no
#                 component or with the free-form one subjects had before 2026-10-20. So a
#                 subject naming agentk and meko releases both
#
# and newest_tag, which prints the product's newest tag (by version, across its prefixes).

product=agentk
_args=''
while [ $# -gt 0 ]; do
  case $1 in
    --product) [ $# -ge 2 ] || { echo "--product needs a value: agentk, meko or present" >&2; exit 2; }; product=$2; shift 2 ;;
    --product=*) product=${1#--product=}; shift ;;
    *) _args="$_args '$(printf '%s' "$1" | sed "s/'/'\\\\''/g")'"; shift ;;
  esac
done
eval "set -- $_args"
unset _args

case $product in
  agentk)
    tag_prefix=agentk-v
    tag_prefixes='agentk-v v'
    scope='(\([^)]*\))?'
    exclude='\((meko|present|presenter)(/[^,)]*)?(,(meko|present|presenter)(/[^,)]*)?)*\)'
    ;;
  meko)
    tag_prefix=meko-v
    tag_prefixes='meko-v'
    scope='\(([^,)]*,)*meko(/[^,)]*)?(,[^)]*)?\)'
    exclude=''
    ;;
  present)
    tag_prefix=present-v
    tag_prefixes='present-v'
    scope='\(([^,)]*,)*present(/[^,)]*)?(,[^)]*)?\)'
    exclude=''
    ;;
  *) echo "unknown product: $product (agentk, meko or present)" >&2; exit 2 ;;
esac

# The product's newest tag of the form <prefix>0.<date>.<n>[-<pre>], or nothing. A release sorts
# above its own prereleases.
newest_tag() {
  for _p in $tag_prefixes; do
    git tag -l "${_p}0.*" | sed -n "s/^\\(${_p}\\)\\([0-9][0-9]*\\.[0-9][0-9]*\\.[0-9][0-9]*\\)\\(-.*\\)\\{0,1\\}\$/\\2 \\3 &/p"
  done | awk '
    {
      split($1, v, ".")
      pre = (NF == 3) ? $2 : ""
      tag = $NF
      if (best == "" || v[1] + 0 > b[1] + 0 ||
          (v[1] + 0 == b[1] + 0 && (v[2] + 0 > b[2] + 0 ||
          (v[2] + 0 == b[2] + 0 && (v[3] + 0 > b[3] + 0 ||
          (v[3] + 0 == b[3] + 0 && bpre != "" && (pre == "" || pre > bpre))))))) {
        best = tag; bpre = pre; b[1] = v[1]; b[2] = v[2]; b[3] = v[3]
      }
    }
    END { if (best != "") print best }'
}
