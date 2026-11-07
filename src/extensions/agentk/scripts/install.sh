#!/bin/sh
# Kode's releases still carry this file, as install.sh, for the Mekos and instructions that run it.
# Meko installs Kode from its release tarball now: `meko extension install kode` (options for
# Kode's setup go after --, e.g. -- --agent kiro). An older Meko runs the newest release's
# install.sh with --meko-url and --meko-key, and refuses one that doesn't mention --meko-url; this
# one only says what to do.
echo "Kode is installed by Meko now. Update Meko first (meko update), then run: meko extension install kode" >&2
exit 1
