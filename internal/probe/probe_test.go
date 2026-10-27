package probe

import "testing"

func TestDelta(t *testing.T) {
	busy := Key{Tgid: 10, Op: uint32(OpRead)}
	idle := Key{Tgid: 20, Op: uint32(OpRead)}
	fresh := Key{Tgid: 30, Op: uint32(OpWrite)}
	reused := Key{Tgid: 40, Op: uint32(OpRead)}

	prev := map[Key]Stats{
		busy:   {Count: 100, Errors: 1, Hist: [40]uint64{3: 100}},
		idle:   {Count: 50},
		reused: {Count: 10, Errors: 9, Hist: [40]uint64{5: 10}},
	}
	cur := map[Key]Stats{
		busy:   {Count: 300, Errors: 5, Hist: [40]uint64{3: 250, 20: 50}},
		idle:   {Count: 50},
		fresh:  {Count: 7},
		reused: {Count: 20, Errors: 0, Hist: [40]uint64{6: 20}}, // more calls, fewer errors
	}

	d := Delta(prev, cur)
	if len(d) != 3 {
		t.Fatalf("got %d keys, want 3 (idle skipped): %v", len(d), d)
	}
	if b := d[busy]; b.Count != 200 || b.Errors != 4 || b.Hist[3] != 150 || b.Hist[20] != 50 {
		t.Errorf("busy delta = %+v", b)
	}
	if d[fresh].Count != 7 {
		t.Errorf("fresh key should count in full, got %+v", d[fresh])
	}
	if r := d[reused]; r.Count != 20 || r.Hist[6] != 20 || r.Hist[5] != 0 {
		t.Errorf("reused PID should restart from zero, got %+v", r)
	}
}
