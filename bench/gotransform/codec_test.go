package main

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestPositionalRoundTrip(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for _, n := range []int{0, 1, 17, 4096, 100000} {
		pred := make([]byte, n)
		r.Read(pred)
		new := append([]byte(nil), pred...)
		for k := 0; k < n/50; k++ {
			new[r.Intn(n)] ^= byte(1 + r.Intn(255))
		}
		if n > 0 {
			new[0] ^= 1
			new[n-1] ^= 1
		}
		got, err := applyPositional(pred, encodePositional(pred, new))
		if err != nil || !bytes.Equal(got, new) {
			t.Fatalf("n=%d: round trip failed: %v", n, err)
		}
		// different lengths: the patch carries the new length
		if n > 4 {
			short := new[:n-3]
			got, err = applyPositional(pred, encodePositional(pred, short))
			if err != nil || !bytes.Equal(got, short) {
				t.Fatalf("n=%d: truncated round trip failed: %v", n, err)
			}
		}
	}
}

func TestDataMapRLE(t *testing.T) {
	m := &DataMap{Block: 16, OldLen: 1000, Delta: []int64{0, 0, 0, 16, 16, -8, -8, -8, 1 << 30, 0}}
	d, err := DecodeDataMapRLE(m.EncodeRLE())
	if err != nil || d.Block != 16 || d.OldLen != 1000 || len(d.Delta) != len(m.Delta) {
		t.Fatalf("decode: %v %+v", err, d)
	}
	for i := range m.Delta {
		if d.Delta[i] != m.Delta[i] {
			t.Fatalf("delta[%d]=%d want %d", i, d.Delta[i], m.Delta[i])
		}
	}
}

func TestShiftTableRoundTrip(t *testing.T) {
	st := &ShiftTable{Offs: []uint64{0, 4096, 1 << 20}, Deltas: []int64{0, 16, -8}}
	d, err := DecodeShiftTable(st.Encode())
	if err != nil || len(d.Offs) != 3 || d.Offs[2] != 1<<20 || d.Deltas[2] != -8 || d.Deltas[1] != 16 {
		t.Fatalf("decode: %v %+v", err, d)
	}
	if d.Map(5000) != 5016 || d.Map(1<<20+8) != 1<<20 {
		t.Fatalf("map: %d %d", d.Map(5000), d.Map(1<<20+8))
	}
}

func TestSampledIndexFindsShift(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	old := make([]byte, 64*1024)
	r.Read(old)
	// new = old with 24 bytes inserted at 20000 and a 16-byte block changed at 40000
	new := append(append(append([]byte(nil), old[:20000]...), make([]byte, 24)...), old[20000:]...)
	for i := 40024; i < 40040; i++ {
		new[i] ^= 0xff
	}
	m := buildDataMap(old, new, 16)
	if m.Delta[0] != 0 || m.Delta[30000/16] != 24 || m.Stats.ShiftChanges != 1 {
		t.Fatalf("map: %s delta0=%d delta@30000=%d", m, m.Delta[0], m.Delta[30000/16])
	}
	if m.Stats.Unmatched != 1 {
		t.Fatalf("expected the one changed block unmatched: %s", m)
	}
}

func TestPatchHeaderRoundTrip(t *testing.T) {
	p := &patchFile{S1Kind: 'h', S2Kind: 'p', NewLen: 12345, Layout: []byte{1, 2, 3}, S1a: []byte{4}, S1b: []byte{7, 8, 9}, S2: []byte{5, 6}}
	p.OldSum[0], p.NewSum[31], p.PredSum[7] = 1, 2, 3
	q, err := parsePatch(p.marshal())
	if err != nil || q.NewLen != 12345 || q.S1Kind != 'h' || q.S2Kind != 'p' || !bytes.Equal(q.S2, []byte{5, 6}) || !bytes.Equal(q.S1b, []byte{7, 8, 9}) || q.PredSum[7] != 3 || q.NewSum[31] != 2 {
		t.Fatalf("round trip: %v %+v", err, q)
	}
}

func TestMajorities(t *testing.T) {
	// votes sorted by (old, new): 3:1 for target 1, unanimous for 2, a tie for 3
	votes := []vote{{1, 10}, {1, 10}, {1, 10}, {1, 11}, {2, 20}, {3, 30}, {3, 30}, {3, 31}, {3, 31}}
	got := majorities(votes)
	want := []majority{{1, 10, 3, 4}, {2, 20, 1, 1}, {3, 30, 2, 4}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("majority %d: got %+v want %+v", i, got[i], want[i])
		}
	}
	if len(majorities(nil)) != 0 {
		t.Fatal("empty votes")
	}
}

func TestOverridesRoundTrip(t *testing.T) {
	ov := []AddrOverride{{0x1000, 0x1000}, {0x1040, 0x1050}, {0x2000, 0x1ff0}, {1 << 40, 1<<40 + 7}}
	w := &wbuf{}
	encodeOverrides(w, ov)
	r := &rbuf{b: w.b}
	got := decodeOverrides(r)
	if r.err != nil || len(got) != len(ov) {
		t.Fatalf("decode: %v %+v", r.err, got)
	}
	for i := range ov {
		if got[i] != ov[i] {
			t.Fatalf("override %d: got %+v want %+v", i, got[i], ov[i])
		}
	}
	if m := overrideMap(got); m[0x2000] != 0x1ff0 || len(m) != 4 {
		t.Fatalf("overrideMap: %v", m)
	}
	if overrideMap(nil) != nil {
		t.Fatal("empty override map must be nil")
	}
	// a truncated stream must fail, not panic
	if r := (&rbuf{b: w.b[:len(w.b)-1]}); len(decodeOverrides(r)) == len(ov) && r.err == nil {
		t.Fatal("truncated stream decoded cleanly")
	}
}

func TestPcTableLen(t *testing.T) {
	// three tables back to back: a leading zero value delta is legal only in
	// the first pair; a multi-byte varint must not be mistaken for the end
	tab := []byte{
		0x00, 0x05, 0x81, 0x01, 0x02, 0x00, // (0,5) (129,2) end -> 6 bytes
		0x02, 0x03, 0x00, // (2,3) end -> 3
		0x7f, 0x80, 0x80, 0x01, 0x00, // (127, 16384) end -> 5
	}
	for off, want := range map[int]int{0: 6, 6: 3, 9: 5} {
		if got := pcTableLen(tab, off); got != want {
			t.Fatalf("pcTableLen at %d: got %d want %d", off, got, want)
		}
	}
	if got := pcTableLen([]byte{0x01, 0x02}, 0); got != 2 { // unterminated: runs to the end
		t.Fatalf("unterminated: %d", got)
	}
}

func TestFillSkeleton(t *testing.T) {
	b := &Bin{Pcln: &Pcln{Data: make([]byte, 64)}}
	ranges := []Range{{8, 4}, {40, 8}}
	data := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	if err := fillSkeleton(b, ranges, data); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b.Pcln.Data[8:12], data[:4]) || !bytes.Equal(b.Pcln.Data[40:48], data[4:]) || b.Pcln.Data[12] != 0 || b.Pcln.Data[39] != 0 {
		t.Fatalf("data: %v", b.Pcln.Data)
	}
	if err := fillSkeleton(b, ranges, data[:11]); err == nil {
		t.Fatal("length mismatch accepted")
	}
}

func TestDenseIndexLookup(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	new := make([]byte, 8192)
	r.Read(new)
	copy(new[4096:], new[:4096]) // every window occurs twice, 4096 apart
	ix := buildWinIndex(new, 16)
	for _, p := range []int{0, 16, 1000, 4080} {
		h := hashWindow(new[p : p+16])
		ents := ix.lookup(h)
		var pos []uint32
		for _, e := range ents {
			if e.h == uint32(h>>32) {
				pos = append(pos, e.p)
			}
		}
		if len(pos) != 2 || pos[0] != uint32(p) || pos[1] != uint32(p+4096) {
			t.Fatalf("window %d: positions %v", p, pos)
		}
	}
}
