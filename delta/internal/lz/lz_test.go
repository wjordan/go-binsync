package lz

import (
	"bytes"
	"math/rand"
	"testing"
)

func roundTrip(t *testing.T, src, dst []byte) int {
	t.Helper()
	ctrl, lit := Emit(src, dst, nil, nil)
	out := make([]byte, len(dst))
	r := &Reader{Ctrl: ctrl, Lit: lit}
	if err := r.Apply(src, out); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !bytes.Equal(out, dst) {
		t.Fatalf("round trip differs (src %d, dst %d)", len(src), len(dst))
	}
	if len(r.Ctrl) != 0 || len(r.Lit) != 0 {
		t.Fatalf("%d ctrl and %d lit bytes left over", len(r.Ctrl), len(r.Lit))
	}
	return len(ctrl) + len(lit)
}

func TestRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	base := make([]byte, 100000)
	r.Read(base)
	cases := map[string][]byte{
		"identical": append([]byte(nil), base...),
		"empty":     {},
		"prefix":    append([]byte(nil), base[:5000]...),
		"inserted":  append(append(append([]byte(nil), base[:40000]...), make([]byte, 37)...), base[40000:]...),
		"deleted":   append(append([]byte(nil), base[:40000]...), base[40100:]...),
		"unrelated": func() []byte { b := make([]byte, 3000); r.Read(b); return b }(),
	}
	for name, dst := range cases {
		t.Run(name, func(t *testing.T) {
			n := roundTrip(t, base, dst)
			// an insertion or a deletion must cost far less than the file
			if name == "inserted" || name == "deleted" || name == "identical" {
				if n > 2000 {
					t.Fatalf("%s cost %d bytes", name, n)
				}
			}
		})
	}
	// empty source
	roundTrip(t, nil, base[:100])
}

func TestApplyRejectsCorruptStreams(t *testing.T) {
	src := bytes.Repeat([]byte("binsync"), 1000)
	dst := append(append([]byte(nil), src[500:]...), src[:500]...)
	ctrl, lit := Emit(src, dst, nil, nil)
	out := make([]byte, len(dst))
	for _, bad := range []struct {
		name string
		r    Reader
	}{
		{"truncated ctrl", Reader{Ctrl: ctrl[:len(ctrl)/2], Lit: lit}},
		{"truncated lit", Reader{Ctrl: ctrl, Lit: lit[:len(lit)/2]}},
		{"empty", Reader{}},
	} {
		if err := bad.r.Apply(src, out); err == nil {
			t.Fatalf("%s: accepted", bad.name)
		}
	}
	// a copy pointing past the source must be refused, not panic
	r := Reader{Ctrl: []byte{0x00, 0xff, 0xff, 0x03, 0x00}, Lit: nil}
	if err := r.Apply(src[:10], out); err == nil {
		t.Fatal("out-of-range copy accepted")
	}
}

func BenchmarkEmit(b *testing.B) {
	r := rand.New(rand.NewSource(1))
	src := make([]byte, 4<<20)
	r.Read(src)
	dst := append(append([]byte(nil), src[:1<<20]...), src[1<<20+64:]...)
	b.SetBytes(int64(len(src)))
	for b.Loop() {
		Emit(src, dst, nil, nil)
	}
}
