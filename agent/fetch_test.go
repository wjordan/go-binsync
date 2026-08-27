package agent

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/wjordan/go-binsync/release"
	"github.com/wjordan/go-binsync/store"
)

// smallFrames republishes data as a blob of frameLen-byte frames. The frame
// table is legal by the pointer's own rules and lets the parallel fetch be
// tested without an 8 MiB release.
func (f *fixture) smallFrames(data []byte, frameLen int) *release.Blob {
	f.t.Helper()
	h := release.HashBytes(data)
	b := &release.Blob{Key: release.BlobKey(h)}
	var obj []byte
	for off := 0; off < len(data); off += frameLen {
		end := min(off+frameLen, len(data))
		z, fr := release.EncodeBlob(h, data[off:end])
		b.Frames = append(b.Frames, release.Frame{
			Off: int64(off), Len: int64(end - off),
			ZOff: int64(len(obj)), ZLen: int64(len(z)),
			B3: fr.Frames[0].B3,
		})
		obj = append(obj, z...)
	}
	b.Size = int64(len(obj))
	f.put(b.Key, obj)
	f.ptr.Head.Blob = b
	f.putPointer()
	return b
}

// TestBlobAssemblesEveryFrame: the parallel fetch puts each frame where the
// table says, and the whole thing hashes to the release.
func TestBlobAssemblesEveryFrame(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r1 := relBytes(1)
	f.publish(r1)
	f.smallFrames(r1, 16<<10)

	a := f.agent(Hooks{})
	got, err := a.fetchBlob(t.Context(), f.ptr.Head)
	if err != nil {
		t.Fatalf("fetchBlob: %v\n%s", err, f.log.dump())
	}
	if !bytes.Equal(got, r1) {
		t.Fatal("the assembled blob is not the release")
	}
}

// TestBlobResumesWhatItAlreadyHas: a retry after a failed pass re-requests
// only the frames it is missing (docs/DESIGN.md 5).
func TestBlobResumesWhatItAlreadyHas(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r1 := relBytes(1)
	f.publish(r1)
	b := f.smallFrames(r1, 32<<10)

	a := f.agent(Hooks{})
	fl := newFlaky(a.st, store.ErrNotFound)
	a.st = fl

	out := make([]byte, len(r1))
	have := make([]bool, len(b.Frames))
	have[0], have[2] = true, true
	if err := a.fetchFrames(t.Context(), b, out, have); err != nil {
		t.Fatal(err)
	}
	if got, want := fl.count(b.Key), len(b.Frames)-2; got != want {
		t.Errorf("%d requests, want %d: the frames already in hand were fetched again", got, want)
	}
	for i, fr := range b.Frames {
		if !have[i] {
			t.Fatalf("frame %d was not recorded as fetched", i)
		}
		got, want := out[fr.Off:fr.Off+fr.Len], r1[fr.Off:fr.Off+fr.Len]
		if i == 0 || i == 2 {
			want = make([]byte, fr.Len) // never asked for, never written
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d landed wrong", i)
		}
	}
}

// TestBlobWaitsForTheUpload is docs/DESIGN.md 4.4 / D13: the pointer goes
// live before the blob is up, and a target that needs it waits.
func TestBlobWaitsForTheUpload(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r1 := relBytes(1)
	f.publish(r1)
	f.install([]byte("something nobody published"))

	a := f.agent(Hooks{})
	fl := newFlaky(a.st, store.ErrNotFound)
	fl.failFor(release.BlobKey(release.HashBytes(r1)), 3)
	a.st = fl

	o, err := a.cycle(t.Context())
	if o != OutcomeUpdated || err != nil {
		t.Fatalf("cycle = %v, %v; want updated\n%s", o, err, f.log.dump())
	}
	if !bytes.Equal(f.binary(), r1) {
		t.Error("the target did not reach the head release")
	}
	if !f.log.has("fetch", "waiting", "the publisher has not uploaded it yet") {
		t.Errorf("the wait was not reported\n%s", f.log.dump())
	}
}

// TestBlobGivesUpAfterTheWait: the wait is bounded, so a publisher that died
// before uploading does not park a target forever.
func TestBlobGivesUpAfterTheWait(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r1 := relBytes(1)
	f.publish(r1)
	f.remove(release.BlobKey(release.HashBytes(r1)))

	a := f.agent(Hooks{})
	a.blobWait = 20 * time.Millisecond
	o, err := a.cycle(t.Context())
	if o != OutcomeError || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cycle = %v, %v; want an error naming the missing blob\n%s", o, err, f.log.dump())
	}
	if f.exists(f.path) {
		t.Error("something was installed without the blob")
	}
}

// TestFrameRetriesATransportError: a dropped connection is retried, unlike a
// 404, which means something else entirely.
func TestFrameRetriesATransportError(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r1 := relBytes(1)
	f.publish(r1)
	f.install([]byte("something nobody published"))

	a := f.agent(Hooks{})
	fl := newFlaky(a.st, io.ErrUnexpectedEOF)
	fl.failFor(release.BlobKey(release.HashBytes(r1)), 2)
	a.st = fl

	if o, err := a.cycle(t.Context()); o != OutcomeUpdated || err != nil {
		t.Fatalf("cycle = %v, %v; want updated\n%s", o, err, f.log.dump())
	}
	if !bytes.Equal(f.binary(), r1) {
		t.Error("the target did not reach the head release")
	}
}

// TestPatchRetriesATransportError: the same for a chain fetch.
func TestPatchRetriesATransportError(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r1, r2 := relBytes(1), relBytes(2)
	from := f.publish(r1)
	f.install(r1)
	to := f.publish(r2)

	a := f.agent(Hooks{})
	fl := newFlaky(a.st, io.ErrUnexpectedEOF)
	fl.failFor(release.PatchKey(from, to), 2)
	a.st = fl

	if o, err := a.cycle(t.Context()); o != OutcomeUpdated || err != nil {
		t.Fatalf("cycle = %v, %v; want updated\n%s", o, err, f.log.dump())
	}
	if !bytes.Equal(f.binary(), r2) {
		t.Error("the target did not reach the head release")
	}
}

// TestMissingPatchIsNotFallenBackFrom: README guarantee 3 says a pointer
// always names an existing patch, so a 404 there is a broken store — loud,
// not silently healed by a full download.
func TestMissingPatchIsNotFallenBackFrom(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r1, r2 := relBytes(1), relBytes(2)
	from := f.publish(r1)
	f.install(r1)
	to := f.publish(r2)
	f.remove(release.PatchKey(from, to))

	a := f.agent(Hooks{})
	o, err := a.cycle(t.Context())
	if o != OutcomeError || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cycle = %v, %v; want an error naming the missing patch\n%s", o, err, f.log.dump())
	}
	if !bytes.Equal(f.binary(), r1) {
		t.Error("the binary changed")
	}
}

// TestCorruptBlobFrameIsRejected: a frame that does not hash to what the
// pointer says is exit code 3.
func TestCorruptBlobFrameIsRejected(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r1 := relBytes(1)
	f.publish(r1)
	f.install([]byte("something nobody published"))

	key := release.BlobKey(release.HashBytes(r1))
	obj := f.get(key)
	obj[len(obj)/2] ^= 0xff
	f.put(key, obj)

	a := f.agent(Hooks{})
	o, err := a.cycle(t.Context())
	if o != OutcomeVerifyFailed || !errors.Is(err, errVerify) {
		t.Fatalf("cycle = %v, %v; want verify-failed\n%s", o, err, f.log.dump())
	}
	if !bytes.Equal(f.binary(), []byte("something nobody published")) {
		t.Error("a corrupt blob was installed")
	}
}

// TestBadFrameTableIsRefused: the frame table decides an allocation and a
// set of writes, so it is checked before either (docs/DESIGN.md 8).
func TestBadFrameTableIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		corrupt func(b *release.Blob)
	}{
		{"a gap between frames", func(b *release.Blob) { b.Frames[0].Off = 64 }},
		{"an overlong frame", func(b *release.Blob) { b.Frames[0].Len = release.FrameSize + 1 }},
		{"stored bytes that do not add up", func(b *release.Blob) { b.Size += 7 }},
		{"a frame table shorter than the release", func(b *release.Blob) { b.Frames[0].Len -= 8 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			r1 := relBytes(1)
			f.publish(r1)
			tc.corrupt(f.ptr.Head.Blob)
			f.putPointer()
			f.install([]byte("something nobody published"))

			o, err := Once(t.Context(), f.config(), Hooks{})
			if o != OutcomeError || err == nil {
				t.Fatalf("Once = %v, %v; want an error about the frame table", o, err)
			}
		})
	}
}

// TestGetRefusesAWrongLength: the pointer says how long an object is, and an
// object that disagrees is refused before it is hashed.
func TestGetRefusesAWrongLength(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r1, r2 := relBytes(1), relBytes(2)
	from := f.publish(r1)
	f.install(r1)
	to := f.publish(r2)

	key := release.PatchKey(from, to)
	f.put(key, append(f.get(key), 0))

	a := f.agent(Hooks{})
	o, err := a.cycle(t.Context())
	if o != OutcomeError || err == nil {
		t.Fatalf("cycle = %v, %v; want an error about the length", o, err)
	}
	if !bytes.Equal(f.binary(), r1) {
		t.Error("the binary changed")
	}
}

// TestPointerReadIsBounded: latest.json is the one object with no declared
// length, so the read is capped by MaxSize.
func TestPointerReadIsBounded(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.publish(relBytes(1))

	a := f.agent(Hooks{})
	a.max = 32
	o, err := a.cycle(t.Context())
	if o != OutcomeError || err == nil {
		t.Fatalf("cycle = %v, %v; want an error about the limit", o, err)
	}
}
