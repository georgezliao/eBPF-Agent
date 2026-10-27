package hist

import (
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	var h [40]uint64
	h[10] = 90 // [1024ns, 2048ns)
	h[20] = 10 // [~1ms, ~2ms)

	cases := []struct {
		q    float64
		want time.Duration
	}{
		{0.5, 2048},
		{0.9, 2048},
		{0.91, 1 << 21},
		{0.99, 1 << 21},
		{1, 1 << 21},
	}
	for _, c := range cases {
		if got := Percentile(h[:], c.q); got != c.want {
			t.Errorf("Percentile(%v) = %v, want %v", c.q, got, c.want)
		}
	}
}

func TestPercentileEmpty(t *testing.T) {
	if got := Percentile(make([]uint64, 40), 0.99); got != 0 {
		t.Errorf("empty histogram: got %v, want 0", got)
	}
}

func TestSub(t *testing.T) {
	a := []uint64{5, 7, 1}
	b := []uint64{2, 7, 0}
	got := Sub(a, b)
	want := []uint64{3, 0, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Sub = %v, want %v", got, want)
		}
	}
}
