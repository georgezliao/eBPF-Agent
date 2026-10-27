package probe

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func encodedEvent(b *testing.B) []byte {
	var buf bytes.Buffer
	e := Event{TsNs: 1, DurationNs: 2, Ret: -3, Tgid: 4, Tid: 5, Op: 1}
	copy(e.Comm[:], []int8{'d', 'd'})
	if err := binary.Write(&buf, binary.LittleEndian, &e); err != nil {
		b.Fatal(err)
	}
	return buf.Bytes()
}

func TestDecodeMatchesBinaryLayout(t *testing.T) {
	var buf bytes.Buffer
	want := Event{TsNs: 1 << 40, DurationNs: 12345, Ret: -11, Tgid: 4, Tid: 5, Op: uint32(OpFsync)}
	copy(want.Comm[:], []int8{'d', 'e', 'm', 'o'})
	if err := binary.Write(&buf, binary.LittleEndian, &want); err != nil {
		t.Fatal(err)
	}
	got, err := Decode(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got != want || got.CommString() != "demo" {
		t.Errorf("Decode = %+v, want %+v", got, want)
	}
	if _, err := Decode(buf.Bytes()[:40]); err == nil {
		t.Error("short record should fail")
	}
}

func BenchmarkDecode(b *testing.B) {
	raw := encodedEvent(b)
	for b.Loop() {
		if _, err := Decode(raw); err != nil {
			b.Fatal(err)
		}
	}
}
