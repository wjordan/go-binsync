package delta

import (
	"encoding/binary"
	"fmt"

	"binsync/delta/internal/lz"
)

// The correction turns a prediction into the real file (docs/DESIGN.md 3.4).
//
// Where the prediction is right -- most of the file -- it costs nothing: the
// stream names a gap and moves on. Where it is wrong, the differing bytes
// form a region, and inside a region the new bytes are re-expressed as
// copies from the prediction's own bytes there plus literals, so a function
// that grew by five bytes costs five literals and a copy instead of its
// whole shifted tail.
//
// Two invariants make the decoder cheap and safe. A region occupies the same
// span in the prediction and in the output, because the prediction is
// length-exact by construction. And a region's source window only ever
// extends *forward* from the region's start, into bytes no earlier region
// has touched -- so the decoder snapshots that window, writes the region in
// place, and never holds prediction and output at the same time.

const (
	// mergeGap is how many identical bytes are cheaper to re-send than to
	// pay for a second region header.
	mergeGap = 6
	// srcSlack is how far past a region its source window reaches, so that
	// content the prediction placed slightly early can still be copied.
	srcSlack = 256
)

// A region is written one of two ways, and the encoder picks whichever is
// smaller: as an lz stream over the prediction's own bytes there, which is
// what makes a function that grew cost its inserted bytes rather than its
// whole shifted tail, or as plain literals, which is cheaper for the many
// two- and four-byte regions a relocation mistake leaves behind. The choice
// is the low bit of the span. The source window's slack is not transmitted:
// it follows from the region's end and the file's length.
const lzRegion = 1

func slackAt(end, n int) int { return min(srcSlack, n-end) }

// encodeCorrection writes the stream that turns pred into want. The two must
// be the same length.
func encodeCorrection(pred, want []byte) ([]byte, error) {
	if len(pred) != len(want) {
		return nil, fmt.Errorf("delta: prediction is %d bytes, target is %d", len(pred), want)
	}
	var ctrl, lit []byte
	var tmp [binary.MaxVarintLen64]byte
	putU := func(v uint64) { ctrl = append(ctrl, tmp[:binary.PutUvarint(tmp[:], v)]...) }

	nregions, prevEnd := 0, 0
	for s := 0; s < len(want); {
		if pred[s] == want[s] {
			s++
			continue
		}
		// grow the region while the identical stretch ahead is shorter than
		// a new region header would cost
		e := s + 1
		for e < len(want) {
			k := e
			for k < len(want) && k-e < mergeGap && pred[k] == want[k] {
				k++
			}
			if k-e < mergeGap && k < len(want) {
				e = k + 1
				continue
			}
			break
		}
		slack := slackAt(e, len(pred))
		putU(uint64(s - prevEnd))
		mark := len(ctrl)
		putU(uint64(e-s)<<1 | lzRegion)
		c2, l2 := lz.Emit(pred[s:e+slack], want[s:e], ctrl, lit)
		if len(c2)-mark+len(l2)-len(lit) < e-s {
			ctrl, lit = c2, l2
		} else {
			ctrl = ctrl[:mark]
			putU(uint64(e-s) << 1)
			lit = append(lit, want[s:e]...)
		}
		nregions++
		prevEnd = e
		s = e
	}

	w := &wbuf{}
	w.u(uint64(len(want)))
	w.u(uint64(nregions))
	w.u(uint64(len(ctrl)))
	w.u(uint64(len(lit)))
	w.raw(ctrl)
	w.raw(lit)
	return w.b, nil
}

// applyCorrection rewrites buf, which holds the prediction, into the real
// file. It allocates only the scratch copy of one region's source window.
func applyCorrection(buf, stream []byte) error {
	r := &rbuf{b: stream}
	n := r.u()
	nregions := r.un(uint64(len(buf))+1, "region count")
	ctrlLen := r.un(uint64(len(stream)), "control stream length")
	litLen := r.un(uint64(len(stream)), "literal stream length")
	ctrl := r.take(ctrlLen)
	lit := r.take(litLen)
	if err := r.done(); err != nil {
		return err
	}
	if n != uint64(len(buf)) {
		return fmt.Errorf("%w: correction is for a %d-byte file, prediction is %d", errCorrupt, n, len(buf))
	}

	var scratch []byte
	pos := 0
	c := &rbuf{b: ctrl}
	rd := &lz.Reader{Lit: lit}
	for i := uint64(0); i < nregions; i++ {
		gap := c.un(uint64(len(buf)), "region gap")
		v := c.un(uint64(len(buf))<<1|1, "region span")
		if c.err != nil {
			return c.err
		}
		span, isLZ := v>>1, v&lzRegion != 0
		if gap > uint64(len(buf)-pos) {
			return fmt.Errorf("%w: region %d starts past the end of the file", errCorrupt, i)
		}
		pos += int(gap)
		if span > uint64(len(buf)-pos) {
			return fmt.Errorf("%w: region %d spans past the end of the file", errCorrupt, i)
		}
		if !isLZ {
			if span > uint64(len(rd.Lit)) {
				return fmt.Errorf("%w: region %d wants %d literal bytes, %d remain", errCorrupt, i, span, len(rd.Lit))
			}
			copy(buf[pos:], rd.Lit[:span])
			rd.Lit = rd.Lit[span:]
			pos += int(span)
			continue
		}
		slack := slackAt(pos+int(span), len(buf))
		src := append(scratch[:0], buf[pos:pos+int(span)+slack]...)
		scratch = src
		rd.Ctrl = c.b
		if err := rd.Apply(src, buf[pos:pos+int(span)]); err != nil {
			return fmt.Errorf("%w: region %d: %v", errCorrupt, i, err)
		}
		c.b = rd.Ctrl
		pos += int(span)
	}
	if len(c.b) != 0 {
		return fmt.Errorf("%w: %d control bytes after the last region", errCorrupt, len(c.b))
	}
	if len(rd.Lit) != 0 {
		return fmt.Errorf("%w: %d literal bytes after the last region", errCorrupt, len(rd.Lit))
	}
	return nil
}
