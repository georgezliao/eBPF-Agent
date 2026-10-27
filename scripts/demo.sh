#!/usr/bin/env bash
# Reproduce a slowdown and let TraceLens find it. Run as root on Linux, or from macOS:
#
#   make build && scripts/linux.sh scripts/demo.sh
#
# bin/demo commits 200 requests/s to a write-ahead log (write + fdatasync). After a few
# healthy seconds, a bulk writer fills the page cache on the same disk, and the WAL's
# fdatasync calls have to wait behind its writeback. The agent watches fsync p99 and
# captures the demo process when it crosses 20 ms.
set -euo pipefail
cd "$(dirname "$0")/.."
out=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null; rm -f /var/tmp/tracelens-bulk' EXIT

bin/demo -rate 200 > "$out/demo.log" &
demo=$!
bin/tracelens agent -listen 127.0.0.1:9104 -trigger 20ms -trigger-op fsync -capture 5s -dir "$out" \
  > "$out/agent.log" 2>&1 &
sleep 3

echo "== healthy: tracelens top =="
{ timeout -s INT 2.5 bin/tracelens top -n 4 || true; } | tail -n +3

echo
echo "== injecting a 3 GB buffered write to /var/tmp =="
dd if=/dev/zero of=/var/tmp/tracelens-bulk bs=1M count=3000 status=none &
sleep 3
echo "== during the bulk write: tracelens top =="
{ timeout -s INT 2.5 bin/tracelens top -n 4 || true; } | tail -n +3
sleep 6

echo
echo "== demo's own view (per-second request latency) =="
cat "$out/demo.log"
echo
echo "== agent log (triggered capture) =="
cat "$out/agent.log"
echo
echo "capture files in $out:"
ls "$out"/*.jsonl
