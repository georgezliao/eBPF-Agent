#!/usr/bin/env bash
# Raw mode at a fixed read rate with different ring buffer sizes: where do drops start?
# Prints CSV. Needs root on Linux (scripts/linux.sh from macOS).
set -euo pipefail
RATES=${RATES:-"500000 1000000 0"}
SIZES=${SIZES:-"16384 65536 262144 1048576 4194304 16777216"}
DURATION=${DURATION:-5s}
PORT=9103
metric() { curl -s "127.0.0.1:$PORT/metrics" | awk -v m="$1" '$1 == m {print $2}'; }
# The agent serves /metrics only after its probes are attached.
ready() { until curl -sf "127.0.0.1:$PORT/metrics" >/dev/null; do sleep 0.1; done; }

echo "ring_bytes,target_rate,achieved_ops_per_sec,events_delivered,events_dropped,drop_pct"
for rate in $RATES; do
  for size in $SIZES; do
    bin/tracelens agent -listen 127.0.0.1:$PORT -export-all -ring "$size" >/dev/null 2>&1 & agent=$!
    ready
    out=$(bin/loadgen -workers 2 -rate "$rate" -duration "$DURATION")
    sleep 0.5 # let the consumer drain what's left
    delivered=$(metric 'tracelens_syscalls_total{op="read"}')
    dropped=$(metric tracelens_events_dropped_total)
    kill "$agent"; wait "$agent" || true
    achieved=$(sed -E 's/.*"ops_per_sec":([0-9.e+]+).*/\1/' <<<"$out")
    # Dropped events can be any op; nearly all are loadgen's reads at these rates.
    awk -v s="$size" -v r="$rate" -v a="$achieved" -v d="$delivered" -v x="$dropped" \
      'BEGIN {printf "%d,%d,%.0f,%d,%d,%.2f\n", s, r, a, d, x, 100 * x / (d + x)}'
  done
done
