#!/usr/bin/env bash
# Is a change slower? Alternates an older tracelens build with the current one at the
# unthrottled rate, so both see the same host conditions, and prints one CSV row per run.
# Run on macOS or Linux from the repo root, with the current build already in bin/:
#
#   benchmarks/ab.sh <git-ref> > benchmarks/results/ab.csv
#
# Env: PAIRS.
set -euo pipefail
ref=$1
PAIRS=${PAIRS:-5}

git worktree add -q bin/abtree "$ref"
trap 'git worktree remove --force bin/abtree; rm -f bin/tracelens-old bin/tracelens-new' EXIT
(cd bin/abtree && GOOS=linux CGO_ENABLED=0 go build -o ../tracelens-old ./cmd/tracelens)
cp bin/tracelens bin/tracelens-new

echo "build,mode,reads,bpf_runs,bpf_seconds,ns_per_run,bpf_ns_per_read"
for _ in $(seq "$PAIRS"); do
  for build in old new; do
    cp "bin/tracelens-$build" bin/tracelens
    scripts/linux.sh env RATE=0 benchmarks/probe_cost.sh | tail -2 | sed "s/^/$build,/"
  done
done
