// Package hist reads the log2 latency histograms built by the BPF program. Slot i counts
// durations in [2^i, 2^(i+1)) nanoseconds (slot 0 also holds 0 ns).
package hist

import "time"

// Percentile returns the upper bound of the slot holding the q-th sample (0 < q <= 1).
// Log2 buckets make this an overestimate by up to 2x, which is enough to tell a 50 µs
// read from a 50 ms one. Exact durations come from trace mode.
func Percentile(h []uint64, q float64) time.Duration {
	var total uint64
	for _, n := range h {
		total += n
	}
	if total == 0 {
		return 0
	}
	rank := uint64(q * float64(total))
	if rank == 0 {
		rank = 1
	}
	var seen uint64
	for i, n := range h {
		seen += n
		if seen >= rank {
			return time.Duration(uint64(1) << (i + 1))
		}
	}
	return time.Duration(uint64(1) << len(h))
}

// Sub returns a - b element-wise, for turning two cumulative snapshots into an interval.
func Sub(a, b []uint64) []uint64 {
	out := make([]uint64, len(a))
	for i := range a {
		out[i] = a[i] - b[i]
	}
	return out
}
