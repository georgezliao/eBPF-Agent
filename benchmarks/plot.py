"""Plot benchmark CSVs from benchmarks/results/. Reads only; run on any machine with
matplotlib:  python3 benchmarks/plot.py benchmarks/results/<run>
"""
import csv
import statistics
import sys
from collections import defaultdict
from pathlib import Path

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt

MODES = {"baseline": "#7f7f7f", "aggregated": "#1f77b4", "raw": "#d62728"}


def rows(path):
    with open(path) as f:
        return list(csv.DictReader(f))


def overhead(run):
    by = defaultdict(list)
    for r in rows(run / "overhead.csv"):
        by[(r["mode"], int(r["target_rate"]))].append(r)
    rates = sorted({rate for _, rate in by}, key=lambda r: (r == 0, r))
    label = lambda r: "max" if r == 0 else f"{r // 1000}k"

    fig, (cpu, thr, p99) = plt.subplots(1, 3, figsize=(14, 4))
    for mode, color in MODES.items():
        med = lambda col: [statistics.median(float(r[col]) for r in by[(mode, rate)]) for rate in rates]
        x = range(len(rates))
        cpu.plot(x, med("agent_cpu_pct"), "o-", color=color, label=mode)
        thr.plot(x, [v / 1e6 for v in med("achieved_ops_per_sec")], "o-", color=color, label=mode)
        p99.plot(x, med("load_p99_ns"), "o-", color=color, label=mode)
    for ax, title in [(cpu, "Agent CPU (% of one core)"), (thr, "Achieved reads/s (millions)"),
                      (p99, "Workload read p99 (ns, sampled)")]:
        ax.set_xticks(range(len(rates)), [label(r) for r in rates])
        ax.set_xlabel("target read rate")
        ax.set_title(title)
        ax.grid(alpha=0.3)
    cpu.legend()
    fig.tight_layout()
    fig.savefig(run / "overhead.png", dpi=120)


def ringsweep(run):
    by = defaultdict(list)
    for r in rows(run / "ringsweep.csv"):
        by[int(r["target_rate"])].append(r)
    fig, ax = plt.subplots(figsize=(6, 4))
    for rate, rs in sorted(by.items(), key=lambda kv: (kv[0] == 0, kv[0])):
        sizes = [int(r["ring_bytes"]) // 1024 for r in rs]
        ax.plot(sizes, [float(r["drop_pct"]) for r in rs], "o-",
                label="max" if rate == 0 else f"{rate // 1000}k reads/s")
    ax.set_xscale("log", base=2)
    ax.set_xlabel("ring buffer size (KiB)")
    ax.set_ylabel("events dropped (%)")
    ax.set_title("Raw mode drops vs ring size")
    ax.grid(alpha=0.3)
    ax.legend()
    fig.tight_layout()
    fig.savefig(run / "ringsweep.png", dpi=120)


if __name__ == "__main__":
    run = Path(sys.argv[1])
    if (run / "overhead.csv").exists():
        overhead(run)
    if (run / "ringsweep.csv").exists():
        ringsweep(run)
