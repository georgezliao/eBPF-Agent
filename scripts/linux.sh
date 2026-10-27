#!/usr/bin/env bash
# Run a command as root on a Linux kernel, from the repo root. On Linux it uses sudo.
# On macOS it runs inside a Lima VM (default name "tracelens"), which mounts the home
# directory at the same path, so bin/ built on the host is visible inside.
#
#   limactl start --name=tracelens template:ubuntu-24.04   # once
#   scripts/linux.sh bin/tracelens top
#
# Lima's home mount is read-only; commands that write files should write under /tmp.
set -euo pipefail
cd "$(dirname "$0")/.."

if [ "$(uname -s)" = Linux ]; then
  exec sudo "$@"
fi
exec limactl shell --workdir "$PWD" "${TRACELENS_VM:-tracelens}" sudo "$@"
