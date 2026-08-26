package delta

import (
	"bytes"
	"math/rand"
	"testing"
)

func checkCorrection(t *testing.T, pred, want []byte) int {
	t.Helper()
	s, err := encodeCorrection(pred, want)
	if err != nil {
		t.Fatal(err)
	}
	buf := append([]byte(nil), pred...)
	if err := applyCorrection(buf, s); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, want) {
		t.Fatalf("correction did not reproduce the target (%d bytes)", len(want))
	}
	return len(s)
}

func TestCorrection(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	base := make([]byte, 200000)
	r.Read(base)

	t.Run("identical", func(t *testing.T) {
		if n := checkCorrection(t, base, base); n > 8 {
			t.Fatalf("an exact prediction cost %d bytes", n)
		}
	})
	t.Run("scattered bytes", func(t *testing.T) {
		want := append([]byte(nil), base...)
		for k := 0; k < 100; k++ {
			want[r.Intn(len(want))] ^= 0xff
		}
		checkCorrection(t, base, want)
	})
	t.Run("shifted function tail", func(t *testing.T) {
		// what a function that grew by five bytes looks like: identical
		// head, five new bytes, then the old bytes shifted along
		want := append([]byte(nil), base...)
		copy(want[50005:], base[50000:len(base)-5])
		copy(want[50000:50005], []byte("HELLO"))
		n := checkCorrection(t, base, want)
		if n > 300 {
			t.Fatalf("a shifted tail cost %d bytes; the local match did not fire", n)
		}
	})
	t.Run("whole file differs", func(t *testing.T) {
		want := make([]byte, len(base))
		r.Read(want)
		checkCorrection(t, base, want)
	})
	t.Run("empty", func(t *testing.T) { checkCorrection(t, nil, nil) })
	t.Run("last byte", func(t *testing.T) {
		want := append([]byte(nil), base...)
		want[len(want)-1] ^= 1
		checkCorrection(t, base, want)
	})
}

func TestCorrectionRejectsCorruptStreams(t *testing.T) {
	pred := bytes.Repeat([]byte("abcdefgh"), 500)
	want := append([]byte(nil), pred...)
	copy(want[100:], "ZZZZ")
	s, err := encodeCorrection(pred, want)
	if err != nil {
		t.Fatal(err)
	}
	for i := range s {
		bad := append([]byte(nil), s...)
		bad[i] ^= 0xff
		buf := append([]byte(nil), pred...)
		// it may legitimately apply (a flipped literal byte), but it must
		// never panic or write outside buf
		_ = applyCorrection(buf, bad)
		if len(buf) != len(pred) {
			t.Fatalf("byte %d: buffer resized", i)
		}
	}
	for _, n := range []int{0, 1, len(s) / 2, len(s) - 1} {
		buf := append([]byte(nil), pred...)
		if err := applyCorrection(buf, s[:n]); err == nil {
			t.Fatalf("truncation to %d bytes accepted", n)
		}
	}
}
