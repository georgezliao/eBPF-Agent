#!/usr/bin/env bash
# Run a benchmark command only while the host is quiet, for shared machines. Waits for the
# 1-minute load average to drop below MAX_LOAD, runs the command, and throws the output away
# and tries again if load went over MAX_LOAD by the end. Prints the kept output and logs
# each attempt on stderr. Run on the host, not in the VM:
#
#   benchmarks/quiet.sh scripts/linux.sh env OPS=tcp REPS=1 benchmarks/paths.sh
#
# Env: MAX_LOAD, TRIES.
set -euo pipefail
MAX_LOAD=${MAX_LOAD:-5}
TRIES=${TRIES:-40}
load() { uptime | sed -E 's/.*load averages?: ([0-9.]+).*/\1/'; }
quiet() { awk -v l="$(load)" -v m="$MAX_LOAD" 'BEGIN {exit !(l < m)}'; }

for try in $(seq "$TRIES"); do
  until quiet; do sleep 30; done
  start=$(load)
  out=$("$@")
  end=$(load)
  if quiet; then
    echo "try $try kept: load $start to $end" >&2
    echo "$out"
    exit 0
  fi
  echo "try $try discarded: load $start to $end" >&2
done
echo "host never stayed quiet" >&2
exit 3
