#!/usr/bin/env bash
# Is a change slower? Alternates an older tracelens build with the current one, so both
# see the same host conditions, and prints the benchmark's CSV with a build column. Each
# run goes through quiet.sh. Run from the repo root, with the current build already in bin/:
#
#   benchmarks/ab.sh <git-ref> [VAR=value...] [benchmark]   > benchmarks/results/ab.csv
#
# The benchmark defaults to benchmarks/probe_cost.sh with RATE=0. Env: PAIRS.
set -euo pipefail
ref=$1
shift
cmd=("$@")
[ ${#cmd[@]} -eq 0 ] && cmd=(RATE=0 benchmarks/probe_cost.sh)
PAIRS=${PAIRS:-5}

git worktree add -q bin/abtree "$ref"
trap 'cp bin/tracelens-new bin/tracelens 2>/dev/null; git worktree remove --force bin/abtree; rm -f bin/tracelens-old bin/tracelens-new' EXIT
(cd bin/abtree && GOOS=linux CGO_ENABLED=0 go build -o ../tracelens-old ./cmd/tracelens)
cp bin/tracelens bin/tracelens-new

header=
for _ in $(seq "$PAIRS"); do
  for build in old new; do
    cp "bin/tracelens-$build" bin/tracelens
    out=$(benchmarks/quiet.sh scripts/linux.sh env "${cmd[@]}")
    [ -z "$header" ] && header=1 && echo "build,$(head -1 <<<"$out")"
    tail -n +2 <<<"$out" | sed "s/^/$build,/"
  done
done
