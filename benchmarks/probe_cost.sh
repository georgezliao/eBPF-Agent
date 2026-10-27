#!/usr/bin/env bash
# Time spent inside the BPF programs per traced read, for aggregated and raw mode.
# Turns on kernel.bpf_stats_enabled, which itself adds two clock reads per program run,
# so these numbers are kept apart from overhead.sh. Prints CSV.
set -euo pipefail
RATE=${RATE:-500000}
DURATION=${DURATION:-5s}
PORT=9102
metric() { curl -s "127.0.0.1:$PORT/metrics" | awk -v m="$1" '$1 == m {print $2}'; }

sysctl -qw kernel.bpf_stats_enabled=1
trap 'sysctl -qw kernel.bpf_stats_enabled=0' EXIT

echo "mode,reads,bpf_runs,bpf_seconds,ns_per_run,bpf_ns_per_read"
for mode in aggregated raw; do
  flags=; [ "$mode" = raw ] && flags=-export-all
  bin/tracelens agent -listen 127.0.0.1:$PORT $flags >/dev/null 2>&1 & agent=$!
  sleep 1
  runs0=$(metric tracelens_bpf_runs_total) secs0=$(metric tracelens_bpf_run_seconds_total)
  out=$(bin/loadgen -workers 2 -rate "$RATE" -duration "$DURATION")
  runs1=$(metric tracelens_bpf_runs_total) secs1=$(metric tracelens_bpf_run_seconds_total)
  kill "$agent"; wait "$agent" || true
  reads=$(sed -E 's/.*"ops":([0-9]+).*/\1/' <<<"$out")
  # All programs (other processes and sched hooks too) divided by loadgen's reads: an
  # upper bound on the BPF time each read costs.
  awk -v m="$mode" -v n="$reads" -v r0="$runs0" -v r1="$runs1" -v s0="$secs0" -v s1="$secs1" \
    'BEGIN {r = r1 - r0; s = s1 - s0; printf "%s,%d,%d,%.4f,%.0f,%.0f\n", m, n, r, s, 1e9 * s / r, 1e9 * s / n}'
done
