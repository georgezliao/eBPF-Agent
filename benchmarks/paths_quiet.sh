#!/usr/bin/env bash
# benchmarks/paths.sh for a machine other work is using: runs one op at a time under
# quiet.sh, so a batch that loses its quiet host is repeated, and numbers the repetitions.
# Prints CSV. Run on the host:
#
#   benchmarks/paths_quiet.sh > benchmarks/results/paths.csv
#
# Env: OPS, MODES, REPS.
set -euo pipefail
OPS=${OPS:-"read write eagain openclose pingpong tcp"}
MODES=${MODES:-baseline previous current offcpu}
REPS=${REPS:-3}

echo "op,rep,mode,ops_per_sec"
for rep in $(seq "$REPS"); do
  for op in $OPS; do
    benchmarks/quiet.sh scripts/linux.sh env OPS="$op" MODES="$MODES" REPS=1 benchmarks/paths.sh |
      tail -n +2 | sed "s/^$op,1,/$op,$rep,/"
  done
done
