# Sourced, not executed. Defines amp_version, which prints one pin from
# amp/versions.txt:
#
#   . "<amp>/scripts/versions.sh"
#   YB_IMAGE="${YB_IMAGE:-$(amp_version YB_IMAGE)}"
#
# The file is read, not sourced, so it cannot set anything else in the caller's
# environment. A missing pin is an error: under `set -e` the assignment above
# fails, rather than defaulting to an empty image.
amp_version() {
    local file value
    file="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/versions.txt"
    value=$(sed -n "s/^$1=//p" "$file")
    if [ -z "$value" ]; then
        echo "amp_version: no $1 in $file" >&2
        return 1
    fi
    printf '%s\n' "$value"
}
