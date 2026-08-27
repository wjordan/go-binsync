package release

import (
	"bytes"
	"runtime"
	"testing"
)

// testFrame is a stand-in for FrameSize. The frame boundaries are what these
// tests are about and a real 8 MiB frame costs a third of a second to
// compress, so they run against encodeBlob with a small frame instead.
const testFrame = 4096

// blobData mixes a compressible run with incompressible bytes, so that a
// multi-frame blob exercises more than one codec.
func blobData(n int) []byte {
	b := make([]byte, n)
	x := uint32(1)
	for i := range b {
		x = x*1664525 + 1013904223
		if i%testFrame < testFrame/2 {
			b[i] = byte(i / 7)
		} else {
			b[i] = byte(x >> 24)
		}
	}
	return b
}

func decodeBlob(t *testing.T, b *Blob, obj []byte) []byte {
	t.Helper()
	var out []byte
	for _, f := range b.Frames {
		plain, err := DecodeFrame(f, obj[f.ZOff:f.ZOff+f.ZLen])
		if err != nil {
			t.Fatalf("frame at %d: %v", f.Off, err)
		}
		if int64(len(out)) != f.Off {
			t.Fatalf("frame at %d follows %d bytes", f.Off, len(out))
		}
		out = append(out, plain...)
	}
	return out
}

func TestBlobRoundTrip(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, testFrame - 1, testFrame, testFrame + 1, 3*testFrame + 7} {
		data := blobData(n)
		h := HashBytes(data)
		obj, b := encodeBlob(h, data, testFrame)

		if want := max((n+testFrame-1)/testFrame, 1); len(b.Frames) != want {
			t.Errorf("%d bytes: %d frames, want %d", n, len(b.Frames), want)
		}
		if b.Size != int64(len(obj)) || b.PlainSize() != int64(n) {
			t.Errorf("%d bytes: Size=%d (object %d), PlainSize=%d", n, b.Size, len(obj), b.PlainSize())
		}
		if err := b.Check(); err != nil {
			t.Errorf("%d bytes: Check: %v", n, err)
		}
		if b.Key != BlobKey(h) {
			t.Errorf("%d bytes: key %q", n, b.Key)
		}
		if got := decodeBlob(t, b, obj); !bytes.Equal(got, data) {
			t.Errorf("%d bytes: round trip produced %d bytes", n, len(got))
		}
	}
}

// TestEncodeBlobFrames covers what the small-frame tests cannot: that the
// exported entry point cuts frames at FrameSize.
func TestEncodeBlobFrames(t *testing.T) {
	t.Parallel()
	data := blobData(64 << 10)
	obj, b := EncodeBlob(HashBytes(data), data)
	if len(b.Frames) != 1 || b.Frames[0].Len != int64(len(data)) {
		t.Fatalf("64 KiB came out as %d frames", len(b.Frames))
	}
	if got := decodeBlob(t, b, obj); !bytes.Equal(got, data) {
		t.Error("round trip through the exported encoder differs")
	}
}

// TestBlobIsDeterministic is what lets a publisher finish a blob that a
// previous run left half written (docs/DESIGN.md 4.4): the frames have to
// come out byte for byte the same, on a machine with a different core count.
func TestBlobIsDeterministic(t *testing.T) {
	data := blobData(5*testFrame + 1000)
	h := HashBytes(data)
	a, ab := encodeBlob(h, data, testFrame)

	prev := runtime.GOMAXPROCS(1)
	c, cb := encodeBlob(h, data, testFrame)
	runtime.GOMAXPROCS(prev)

	if !bytes.Equal(a, c) {
		t.Fatal("the object depends on how many workers compressed it")
	}
	for i := range ab.Frames {
		if ab.Frames[i] != cb.Frames[i] {
			t.Fatalf("frame %d differs: %+v vs %+v", i, ab.Frames[i], cb.Frames[i])
		}
	}
}

func TestDecodeFrameRejects(t *testing.T) {
	t.Parallel()
	data := blobData(2 * testFrame)
	obj, b := encodeBlob(HashBytes(data), data, testFrame)
	f := b.Frames[0]
	z := obj[f.ZOff : f.ZOff+f.ZLen]

	if _, err := DecodeFrame(f, z[:len(z)-1]); err == nil {
		t.Error("a short frame must not decode")
	}
	bad := bytes.Clone(z)
	bad[len(bad)/2] ^= 0xff
	if _, err := DecodeFrame(f, bad); err == nil {
		t.Error("a corrupted frame must fail its hash")
	}
	// A frame whose declared plain length is wrong must not be trusted
	// either: Len is what the decoder allocates against.
	short := f
	short.Len--
	if _, err := DecodeFrame(short, z); err == nil {
		t.Error("a frame that decompresses to the wrong length must fail")
	}
	unknown := f
	unknown.Codec = 200
	if _, err := DecodeFrame(unknown, z); err == nil {
		t.Error("an unknown codec must fail")
	}
}

// TestCheckRejects: the frame table arrives in the pointer, which is the one
// object an attacker who can write the store controls. Check is what stops a
// forged table before a fetcher allocates or writes against it.
func TestCheckRejects(t *testing.T) {
	t.Parallel()
	data := blobData(2*testFrame + 10)
	_, good := encodeBlob(HashBytes(data), data, testFrame)

	for name, mutate := range map[string]func(*Blob){
		"a gap between frames":    func(b *Blob) { b.Frames[1].Off += 8 },
		"overlapping frames":      func(b *Blob) { b.Frames[1].Off -= 8 },
		"a frame larger than max": func(b *Blob) { b.Frames[0].Len = FrameSize + 1 },
		"a negative length":       func(b *Blob) { b.Frames[0].Len = -1 },
		"a negative stored size":  func(b *Blob) { b.Frames[0].ZLen = -1 },
		"a hole in the object":    func(b *Blob) { b.Frames[1].ZOff++ },
		"a size that disagrees":   func(b *Blob) { b.Size++ },
	} {
		bad := *good
		bad.Frames = append([]Frame{}, good.Frames...)
		mutate(&bad)
		if err := bad.Check(); err == nil {
			t.Errorf("Check accepted %s", name)
		}
	}
	if err := good.Check(); err != nil {
		t.Errorf("Check rejected a good table: %v", err)
	}
}
