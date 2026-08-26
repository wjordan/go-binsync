package delta

import (
	"fmt"

	"binsync/delta/gobin"
	"binsync/delta/x86"
)

// The Go-aware transform. The patch body is four streams:
//
//	layout    the new file's shape: section table, moduledata values,
//	          function order and sizes, pclntab table offsets, the content
//	          maps and the shift tables (delta/layout.go)
//	stage 1a  a plain delta of funcnametab+filetab, which change length
//	stage 1b  a plain delta of the predicted cutab+pctab+go:func.* against
//	          the real ones -- the residual is shifted, not positional, so
//	          a bounded local window cannot follow it (docs/DESIGN.md 3.4)
//	stage 2   a positional correction of the predicted whole file
//
// plus the BLAKE3 of the predicted file, so that an encoder and a decoder
// that disagree say so instead of producing a wrong binary quietly. The
// four streams are compressed as one frame: a frame per stream would cost a
// 32-byte hash and a table entry each and buys nothing -- compressed
// separately they come to within 0.1 % of the same total.
func encodeGoAMD64(old, new []byte, o Options, st *Stats) ([]byte, error) {
	ob, err := gobin.Parse(old)
	if err != nil {
		return nil, asUnsupported("old", err)
	}
	nb, err := gobin.Parse(new)
	if err != nil {
		return nil, asUnsupported("new", err)
	}
	m := matchFuncs(ob, nb)
	st.Funcs = len(nb.Funcs)
	st.Matched = m.Exact + m.Norm + m.Content
	st.NewFuncs = m.Unmatched

	dmaps, shifts := buildMaps(ob, nb, m)
	ov := deriveOverrides(ob, nb, m, dmaps, shifts)
	layRaw := buildLayout(ob, nb, m, dmaps, shifts, ov).encode(ob)

	// From here the encoder runs the decoder's code on the decoder's inputs:
	// the layout as it will be decoded, and a skeleton built from it. That
	// is what guarantees the prediction the correction is measured against
	// is the prediction the decoder will produce.
	skel, m2, err := skeletonFrom(ob, layRaw)
	if err != nil {
		return nil, err
	}
	lay, _ := decodeLayout(layRaw, ob)

	s1aNew := stage1aBlobs(nb)
	s1a := plainDiff(stage1aBlobs(ob), s1aNew)
	if err := fillTables(skel, stage1aRanges(skel.Pcln), s1aNew); err != nil {
		return nil, err
	}

	mp := newMapper(ob, skel, m2, lay)
	bp := predictBlobs(ob, skel, m2, mp)
	s1bNew := stage1bBlobs(nb)
	s1b := plainDiff(bp.concat(), s1bNew)
	if err := fillTables(skel, stage1bRanges(skel.Pcln), s1bNew); err != nil {
		return nil, err
	}
	mp.blobs = bp

	var xst x86.Stats
	pred := predictWhole(ob, skel, lay, mp, &xst)
	st.PredictErr = diffBytes(pred, new)
	st.Notes = append(st.Notes, fmt.Sprintf("%d insns, %d undecodable bytes, %d unrelocatable refs",
		xst.Insns, xst.Fails, xst.Unknown+xst.NoFit))
	s2, err := encodeCorrection(pred, new)
	if err != nil {
		return nil, err
	}

	st.Layout, st.Stage1a, st.Stage1b, st.Stage2 = len(layRaw), len(s1a), len(s1b), len(s2)
	w := &wbuf{}
	w.bytes(layRaw)
	w.u(uint64(len(s1aNew)))
	w.bytes(s1a)
	// The predicted 1b blobs are padded to the new tables' lengths but never
	// truncated, so a release that shrank a table leaves the prediction
	// longer than the truth; the decoder is told what to expect.
	w.u(uint64(len(s1bNew)))
	w.bytes(s1b)
	sum := hashOf(pred)
	w.raw(sum[:])
	w.raw(s2)
	return w.b, nil
}

func applyGoAMD64(old, body []byte, h *Header) ([]byte, error) {
	ob, err := gobin.Parse(old)
	if err != nil {
		return nil, fmt.Errorf("delta: this patch needs a Go binary the codec understands: %w", err)
	}
	r := &rbuf{b: body}
	layRaw := r.bytes()
	s1aLen := r.un(uint64(h.NewSize), "stage 1a table length")
	s1a := r.bytes()
	s1bLen := r.un(uint64(h.NewSize), "stage 1b table length")
	s1b := r.bytes()
	var sum Hash
	copy(sum[:], r.take(32))
	if r.err != nil {
		return nil, r.err
	}
	s2 := r.b

	skel, m, err := skeletonFrom(ob, layRaw)
	if err != nil {
		return nil, err
	}
	lay, err := decodeLayout(layRaw, ob)
	if err != nil {
		return nil, err
	}
	s1aNew, err := plainPatch(stage1aBlobs(ob), s1a, int64(s1aLen))
	if err != nil {
		return nil, err
	}
	if err := fillTables(skel, stage1aRanges(skel.Pcln), s1aNew); err != nil {
		return nil, err
	}

	mp := newMapper(ob, skel, m, lay)
	bp := predictBlobs(ob, skel, m, mp)
	s1bNew, err := plainPatch(bp.concat(), s1b, int64(s1bLen))
	if err != nil {
		return nil, err
	}
	if err := fillTables(skel, stage1bRanges(skel.Pcln), s1bNew); err != nil {
		return nil, err
	}
	mp.blobs = bp

	pred := predictWhole(ob, skel, lay, mp, nil)
	if hashOf(pred) != sum {
		return nil, fmt.Errorf("delta: the predicted base does not match the encoder's; " +
			"encoder and decoder disagree, fetch the blob")
	}
	if err := applyCorrection(pred, s2); err != nil {
		return nil, err
	}
	return pred, nil
}

// skeletonFrom decodes a layout and builds the decoder's view of the new
// binary from it. Both sides call it, on the same bytes.
func skeletonFrom(old *gobin.Bin, layRaw []byte) (*gobin.Bin, *match, error) {
	l, err := decodeLayout(layRaw, old)
	if err != nil {
		return nil, nil, err
	}
	return skeleton(old, l)
}

func newMapper(old, skel *gobin.Bin, m *match, l *layout) *mapper {
	return &mapper{
		src: old, dst: skel, m: m,
		srcToDst: m.OldToNew, dstToSrc: m.NewToOld,
		dataMaps: l.DataMaps, shifts: l.Shifts, overrides: overrideMap(l.Overrides),
	}
}

// asUnsupported turns a gobin rejection into the codec's own sentinel, so
// that Encode falls back to the plain codec instead of failing.
func asUnsupported(which string, err error) error {
	var u *gobin.Unsupported
	if errorAs(err, &u) {
		return unsupported("%s binary: %s", which, u.Reason)
	}
	return unsupported("%s binary: %v", which, err)
}

func diffBytes(a, b []byte) int {
	n := 0
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			n++
		}
	}
	return n + max(len(a), len(b)) - min(len(a), len(b))
}
