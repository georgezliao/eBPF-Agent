#!/usr/bin/env bash
# Overhead of observing: run loadgen at several read-syscall rates with
#   baseline    no agent
#   aggregated  agent in monitor mode (counters and histograms in BPF maps)
#   raw         agent -export-all (every call through the ring buffer, aggregated in Go)
# and print one CSV row per run. Needs root on Linux; from macOS use scripts/linux.sh:
#
#   scripts/linux.sh benchmarks/overhead.sh > benchmarks/results/overhead.csv
#
# Env: RATES (reads/s, 0 = unthrottled), REPS, DURATION, WORKERS, RING (bytes, raw mode).
set -euo pipefail

RATES=${RATES:-"100000 250000 500000 1000000 0"}
REPS=${REPS:-3}
DURATION=${DURATION:-5s}
WORKERS=${WORKERS:-2}
RING=${RING:-0}
PORT=9101
TCK=$(getconf CLK_TCK)

# CPU ticks (user + system) used so far by a process.
ticks() { awk '{print $14 + $15}' "/proc/$1/stat"; }
metric() { curl -s "127.0.0.1:$PORT/metrics" | awk -v m="$1" '$1 == m {print $2}'; }
json() { sed -E "s/.*\"$1\":([0-9.e+]+).*/\1/" <<<"$2"; }

echo "mode,target_rate,rep,achieved_ops_per_sec,load_p50_ns,load_p99_ns,agent_cpu_pct,events_dropped"
for rate in $RATES; do
  for rep in $(seq "$REPS"); do
    for mode in baseline aggregated raw; do
      agent=
      case $mode in
        aggregated) bin/tracelens agent -listen 127.0.0.1:$PORT >/dev/null 2>&1 & agent=$! ;;
        raw) bin/tracelens agent -listen 127.0.0.1:$PORT -export-all -ring "$RING" >/dev/null 2>&1 & agent=$! ;;
      esac
      [ -n "$agent" ] && sleep 1 # let the probes attach

      before=0; [ -n "$agent" ] && before=$(ticks "$agent")
      start=$(date +%s.%N)
      out=$(bin/loadgen -workers "$WORKERS" -rate "$rate" -duration "$DURATION")
      end=$(date +%s.%N)

      cpu=0 dropped=0
      if [ -n "$agent" ]; then
        cpu=$(awk -v a="$before" -v b="$(ticks "$agent")" -v s="$start" -v e="$end" -v t="$TCK" \
          'BEGIN {printf "%.1f", 100 * (b - a) / t / (e - s)}')
        dropped=$(metric tracelens_events_dropped_total)
        kill "$agent"; wait "$agent" || true
      fi
      echo "$mode,$rate,$rep,$(json ops_per_sec "$out"),$(json p50_ns "$out"),$(json p99_ns "$out"),$cpu,$dropped"
    done
  done
done
