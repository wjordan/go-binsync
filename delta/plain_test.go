package delta

import (
	"bytes"
	"math/rand"
	"testing"
)

func encodeDecode(t *testing.T, old, new []byte, o Options) int {
	t.Helper()
	p, err := Encode(old, new, o)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Apply(old, p, &buf); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), new) {
		t.Fatal("patch did not reproduce the new file")
	}
	return len(p)
}

func TestPlainCodec(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	old := make([]byte, 300000)
	r.Read(old)
	o := Options{PlainOnly: true}

	t.Run("identical", func(t *testing.T) {
		if n := encodeDecode(t, old, old, o); n > 200 {
			t.Fatalf("identical files cost %d bytes", n)
		}
	})
	t.Run("inserted run", func(t *testing.T) {
		new := append(append(append([]byte(nil), old[:100000]...), bytes.Repeat([]byte{0x42}, 64)...), old[100000:]...)
		if n := encodeDecode(t, old, new, o); n > 500 {
			t.Fatalf("one insertion cost %d bytes", n)
		}
	})
	t.Run("shifted with edits", func(t *testing.T) {
		new := append(append([]byte(nil), old[:50]...), old...)
		for k := 0; k < 200; k++ {
			new[r.Intn(len(new))] ^= 0xff
		}
		if n := encodeDecode(t, old, new, o); n > 4000 {
			t.Fatalf("a shift plus 200 edits cost %d bytes", n)
		}
	})
	t.Run("unrelated", func(t *testing.T) {
		new := make([]byte, 40000)
		r.Read(new)
		encodeDecode(t, old, new, o)
	})
	t.Run("empty old", func(t *testing.T) { encodeDecode(t, nil, old[:1000], o) })
	t.Run("empty new", func(t *testing.T) { encodeDecode(t, old, nil, o) })
	t.Run("both empty", func(t *testing.T) { encodeDecode(t, nil, nil, o) })
}

func TestApplyRejectsTampering(t *testing.T) {
	r := rand.New(rand.NewSource(4))
	old := make([]byte, 20000)
	r.Read(old)
	new := append(append([]byte(nil), old[:10000]...), old...)
	p, err := Encode(old, new, Options{PlainOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("wrong old file", func(t *testing.T) {
		if err := Apply(new, p, &bytes.Buffer{}); err == nil {
			t.Fatal("accepted the wrong old file")
		}
	})
	t.Run("every single-byte flip is caught or harmless", func(t *testing.T) {
		// a flipped byte must never produce a wrong file that Apply accepts
		for i := 0; i < len(p); i += 7 {
			bad := append([]byte(nil), p...)
			bad[i] ^= 0x80
			var buf bytes.Buffer
			if err := Apply(old, bad, &buf); err == nil && !bytes.Equal(buf.Bytes(), new) {
				t.Fatalf("byte %d: a corrupt patch produced a wrong file with no error", i)
			}
		}
	})
	t.Run("truncation", func(t *testing.T) {
		for _, n := range []int{0, 4, 40, len(p) / 2, len(p) - 1} {
			if err := Apply(old, p[:n], &bytes.Buffer{}); err == nil {
				t.Fatalf("truncation to %d accepted", n)
			}
		}
	})
}

// TestPlainRoundTripFuzz drives plainDiff/plainPatch over pairs built the way
// a real pair is built -- the same bytes with runs inserted, deleted and
// overwritten -- because the control loop's forward/backward extensions and
// its overlap split are exactly where an off-by-one stops covering the tail.
func TestPlainRoundTripFuzz(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewSource(1))
	base := make([]byte, 200<<10)
	for i := range base {
		// compressible, self-similar bytes: a random file has no matches to
		// get wrong
		base[i] = byte(r.Intn(8) * 17)
	}
	for c := range 200 {
		old := mutate(r, base, r.Intn(12))
		new := mutate(r, old, r.Intn(12))
		got, err := plainPatch(old, plainDiff(old, new), int64(len(new)))
		if err != nil {
			t.Fatalf("case %d (old %d, new %d): %v", c, len(old), len(new), err)
		}
		if !bytes.Equal(got, new) {
			t.Fatalf("case %d (old %d, new %d): round trip differs", c, len(old), len(new))
		}
	}
}

// mutate returns b with n random edits applied.
func mutate(r *rand.Rand, b []byte, n int) []byte {
	out := append([]byte(nil), b...)
	for range n {
		if len(out) == 0 {
			break
		}
		at := r.Intn(len(out))
		size := 1 + r.Intn(4096)
		switch r.Intn(3) {
		case 0: // delete
			out = append(out[:at], out[min(at+size, len(out)):]...)
		case 1: // insert
			ins := make([]byte, size)
			for i := range ins {
				ins[i] = byte(r.Intn(256))
			}
			out = append(out[:at], append(ins, out[at:]...)...)
		default: // overwrite
			for i := at; i < min(at+size, len(out)); i++ {
				out[i] = byte(r.Intn(256))
			}
		}
	}
	return out
}
