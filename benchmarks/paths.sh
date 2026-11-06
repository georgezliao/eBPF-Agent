#!/usr/bin/env bash
# What do the hooks cost off the read path? Runs each loadgen op unthrottled with
#   baseline  no tracing
#   previous  bin/tracelens-old, an older agent build to compare with (optional)
#   current   bin/tracelens agent
#   offcpu    bin/tracelens offcpu following the load, which also attaches every hook
# and prints one CSV row per run. Needs root on Linux:
#
#   scripts/linux.sh benchmarks/paths.sh > benchmarks/results/paths.csv
#
# Env: OPS, REPS, DURATION, WORKERS.
set -euo pipefail

OPS=${OPS:-"read write eagain openclose pingpong tcp"}
REPS=${REPS:-3}
DURATION=${DURATION:-5}
WORKERS=${WORKERS:-2}
PORT=9105
ready() { until curl -sf "127.0.0.1:$PORT/metrics" >/dev/null; do sleep 0.1; done; }

modes="baseline current offcpu"
[ -x bin/tracelens-old ] && modes="baseline previous current offcpu"

echo "op,rep,mode,ops_per_sec"
for rep in $(seq "$REPS"); do
  for op in $OPS; do
    for mode in $modes; do
      agent=
      case $mode in
        previous) bin/tracelens-old agent -listen 127.0.0.1:$PORT >/dev/null 2>&1 & agent=$!; ready ;;
        current) bin/tracelens agent -listen 127.0.0.1:$PORT >/dev/null 2>&1 & agent=$!; ready ;;
      esac
      out=$(mktemp)
      bin/loadgen -op "$op" -workers "$WORKERS" -duration "${DURATION}s" >"$out" & load=$!
      if [ "$mode" = offcpu ]; then
        bin/tracelens offcpu -pid "$load" -duration "$((DURATION + 1))s" >/dev/null &
        agent=$!
      fi
      wait "$load"
      [ -n "$agent" ] && { kill "$agent" 2>/dev/null; wait "$agent" || true; }
      rate=$(sed -E 's/.*"ops_per_sec":([0-9.e+]+).*/\1/' "$out")
      rm -f "$out"
      echo "$op,$rep,$mode,$rate"
    done
  done
done
