#!/usr/bin/env bash
# Inject three faults into a small service and let TraceLens tell them apart. Run as root
# on Linux, or from macOS:
#
#   make build && scripts/linux.sh scripts/demo.sh
#
# bin/client sends 200 commits/s to bin/store, which appends each to a log with write +
# fdatasync. Each fault runs for a few seconds, then gets removed:
#   1. the store sleeps 30 ms per request, as if a dependency got slow
#   2. loopback drops 5% of packets
#   3. a 3 GB buffered write fills the page cache on the same disk
set -euo pipefail
cd "$(dirname "$0")/.."
out=$(mktemp -d)
cleanup() {
  kill $(jobs -p) 2>/dev/null || true
  tc qdisc del dev lo root 2>/dev/null || true
  rm -f /var/tmp/tracelens-bulk /var/tmp/tracelens-demo.wal
}
trap cleanup EXIT
# Writeback left over from an earlier run would slow the log before the fault starts.
sync

show_top() { { timeout -s INT 3.5 bin/tracelens top -interval 3s -n 10 || true; } | grep -E 'PID|store|client'; }

bin/store > "$out/store.log" &
store=$!
bin/client > "$out/client.log" &
sleep 1
bin/tracelens agent -listen 127.0.0.1:9104 -trigger 20ms -trigger-op fsync -capture 3s -dir "$out" \
  > "$out/agent.log" 2>&1 &
sleep 3

echo "== healthy =="
show_top

echo
echo "== fault 1: store takes 30 ms longer per request =="
curl -s -X POST '127.0.0.1:9201/delay?d=30ms'
sleep 2
show_top
curl -s -X POST '127.0.0.1:9201/delay?d=0s'
sleep 2

echo
echo "== fault 2: loopback drops 5% of packets =="
tc qdisc add dev lo root netem loss 5%
sleep 2
show_top
tc qdisc del dev lo root
sleep 2

echo
echo "== fault 3: 3 GB buffered write to the same disk =="
dd if=/dev/zero of=/var/tmp/tracelens-bulk bs=1M count=3000 status=none &
sleep 1
bin/tracelens offcpu -pid "$store" -duration 3s -n 1 -match ext4_sync_file > "$out/offcpu.txt" &
show_top
wait %+ 2>/dev/null || true
sleep 3

echo
echo "== store's fdatasync sleeps during fault 3: tracelens offcpu =="
cat "$out/offcpu.txt"
echo
echo "== client's own view (per-second request latency) =="
cat "$out/client.log"
echo
echo "== agent log (triggered capture) =="
cat "$out/agent.log"
