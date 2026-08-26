// Package delta is binsync's patch codec.
//
// A patch turns the bytes of one binary into the bytes of another. Two
// transforms share one container (docs/DESIGN.md 3.5): a Go-aware
// predict-then-correct codec for the binaries binsync exists for, and a
// bsdiff-class codec for everything else. Both are verified by hash before
// their output is used, so a mispredicting encoder costs bytes, never
// correctness.
package delta

import (
	"fmt"
	"io"
	"runtime"
)

// Options controls Encode. The zero value is the right choice for a
// publisher with no old decoders in the fleet.
type Options struct {
	// MaxTransform is the newest transform the *deployed decoder* is known
	// to support. The publisher reads it from the old binary's build info
	// (docs/DESIGN.md 3.6). Zero means "no limit": use the best available.
	MaxTransform int

	// PlainOnly skips the Go-aware codec even for a supported binary. It
	// exists for the self-check corpus and for `binsync diff --plain`.
	PlainOnly bool

	// Workers bounds the encoder's parallelism. Zero means GOMAXPROCS.
	// It does not affect the bytes produced.
	Workers int

	// Stats, if non-nil, receives a breakdown of where the patch bytes went.
	Stats *Stats
}

// Stats is an accounting of one encode, for the benchmark harness and for
// `binsync diff -v`.
//
// The per-stream sizes are the streams as the codec produced them, before
// compression: the body is compressed as one piece, because the streams
// share enough context that splitting them costs more than it saves.
type Stats struct {
	Transform byte

	Layout  int
	Stage1a int
	Stage1b int
	Stage2  int
	Body    int // the four together, plus the fixed fields between them
	Header  int // the compressed patch, less the body
	Total   int // the compressed patch

	Funcs      int
	Matched    int
	NewFuncs   int
	PredictErr int // bytes where the prediction differed from the new file
	Notes      []string
}

func (o Options) workers() int {
	if o.Workers > 0 {
		return o.Workers
	}
	return runtime.GOMAXPROCS(0)
}

func (o Options) transformCap() int {
	if o.MaxTransform <= 0 {
		return maxTransform
	}
	return min(o.MaxTransform, maxTransform)
}

// Encode produces a patch turning old into new.
//
// It tries the Go-aware transform when both binaries qualify and the
// deployed decoder can read it, and falls back to the plain codec — never
// to an error — when they do not. The returned patch always reproduces new
// exactly; Apply verifies that before it hands the bytes over.
func Encode(old, new []byte, o Options) ([]byte, error) {
	h := &Header{
		From:    hashOf(old),
		To:      hashOf(new),
		OldSize: int64(len(old)),
		NewSize: int64(len(new)),
	}
	st := o.Stats
	if st == nil {
		st = &Stats{}
	}
	var body []byte
	var err error
	if !o.PlainOnly && o.transformCap() >= TransformGoAMD64 {
		body, err = encodeGoAMD64(old, new, o, st)
		if err == nil {
			h.Transform = TransformGoAMD64
		} else if !isUnsupported(err) {
			return nil, err
		} else {
			st.Notes = append(st.Notes, "plain codec: "+err.Error())
		}
	}
	if body == nil {
		if body, err = encodePlain(old, new, o, st); err != nil {
			return nil, err
		}
		h.Transform = TransformPlain
	}
	st.Transform = h.Transform
	st.Body = len(body)
	h.Frames, body = frameBody(body)
	patch := marshalHeader(h, body)
	st.Header = len(patch) - len(body)
	st.Total = len(patch)
	return patch, nil
}

// Apply reconstructs the new binary from old and patch and writes it to w.
//
// The patch is untrusted input: every offset and length in it is checked,
// the predicted base is compared with the hash the encoder recorded, and
// the output is compared with the release hash before a byte reaches w.
func Apply(old, patch []byte, w io.Writer) error {
	h, err := ParseHeader(patch)
	if err != nil {
		return err
	}
	if int64(len(old)) != h.OldSize {
		return fmt.Errorf("delta: old file is %d bytes, patch expects %d", len(old), h.OldSize)
	}
	if got := hashOf(old); got != h.From {
		return fmt.Errorf("delta: old file hashes to %s, patch expects %s", got, h.From)
	}
	body, err := readBody(h, patch)
	if err != nil {
		return err
	}
	var out []byte
	switch h.Transform {
	case TransformPlain:
		out, err = applyPlain(old, body, h)
	case TransformGoAMD64:
		out, err = applyGoAMD64(old, body, h)
	default:
		return &ErrUnsupportedTransform{h.Transform}
	}
	if err != nil {
		return err
	}
	if int64(len(out)) != h.NewSize {
		return fmt.Errorf("%w: output is %d bytes, header says %d", errCorrupt, len(out), h.NewSize)
	}
	if got := hashOf(out); got != h.To {
		return fmt.Errorf("delta: output hashes to %s, patch promises %s", got, h.To)
	}
	_, err = w.Write(out)
	return err
}

// unsupportedError marks an input the Go-aware codec declines. It is never
// a failure: the encoder falls back to the plain codec and the decoder to
// the blob.
type unsupportedError struct{ msg string }

func (e *unsupportedError) Error() string { return e.msg }

func unsupported(f string, a ...any) error { return &unsupportedError{fmt.Sprintf(f, a...)} }

func isUnsupported(err error) bool {
	var u *unsupportedError
	return errorAs(err, &u)
}
