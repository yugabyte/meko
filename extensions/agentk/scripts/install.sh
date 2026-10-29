#!/bin/sh
# AgentK's releases still carry this file, as install.sh, for the Mekos and instructions that run it.
# Meko installs AgentK from its release tarball now: `meko extension install agentk` (options for
# AgentK's setup go after --, e.g. -- --agent kiro). An older Meko runs the newest release's
# install.sh with --meko-url and --meko-key, and refuses one that doesn't mention --meko-url; this
# one only says what to do.
echo "AgentK is installed by Meko now. Update Meko first (meko update), then run: meko extension install agentk" >&2
exit 1
