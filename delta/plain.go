package delta

import (
	"fmt"

	"github.com/wjordan/go-binsync/delta/internal/lz"
)

// The plain codec (transform 0) is what everything that is not a supported
// Go binary gets: another toolchain, arm64, a PIE, a non-Go executable, a
// tarball. It is bsdiff's shape -- an approximate match extended over the
// bytes around it, so that a shifted region costs a control triple and a
// difference stream that is zero wherever the two agree -- with two
// substitutions.
//
// The suffix array is replaced by the same position-ordered index the rest
// of the codec uses. That is what makes bsdiff need 9-12x the input in RAM
// and minutes above 100 MB; the index costs 5x and seconds, and on
// executables the anchors it finds are long enough that the extension
// heuristic does the same work.
//
// And the difference stream is stored as runs of non-zero bytes rather than
// densely. bsdiff's stream is one byte per matched byte and is almost all
// zero -- 383 K non-zero in 30 MB on the reference pair -- which bzip2's
// block sort handles beautifully and every LZ compressor handles badly.
// Carrying the zeros as gaps instead of bytes takes that pair from 315 KB
// to 170 KB against bsdiff's 151 KB, and drops the decoder's working set
// from the size of the file to nothing.

// MaxPlainSize is the largest input the plain codec accepts. Above it,
// publish uploads the blob only and says so.
const MaxPlainSize = 256 << 20

// plainProbe is how hard the anchor search looks; it stands in for the
// suffix array's exact longest match.
const plainProbe = 5

// diffMergeGap is how many zero bytes are cheaper to carry as bytes than to
// pay for a second run header. Measured on the corpus: 6.
const diffMergeGap = 6

func encodePlain(old, new []byte, o Options, st *Stats) ([]byte, error) {
	if len(old) > MaxPlainSize {
		return nil, fmt.Errorf("delta: the plain codec is capped at %d MB, old file is %d MB", MaxPlainSize>>20, len(old)>>20)
	}
	b := plainDiff(old, new)
	st.Stage2 = len(b)
	return b, nil
}

// plainDiff is the codec proper: a control stream of (match length, literal
// length, source seek) triples, a sparse stream of the differences over the
// matched runs, and the literals. It is also how the Go-aware codec sends
// its stage-1a tables, which change length and so cannot use a positional
// correction.
func plainDiff(old, new []byte) []byte {
	var ctrl, diff, extra wbuf
	d := &diffWriter{w: &diff}
	if len(old) == 0 {
		ctrl.u(0)
		ctrl.u(uint64(len(new)))
		ctrl.s(0)
		extra.raw(new)
		return packPlain(1, &ctrl, &diff, &extra)
	}
	ix := lz.NewIndex(old)
	ix.SetProbe(plainProbe)

	inOld := func(i int) bool { return i >= 0 && i < len(old) }
	scan, mlen, pos := 0, 0, 0
	lastscan, lastpos, lastoffset := 0, 0, 0
	ntriples := 0
	for scan < len(new) {
		oldscore := 0
		scan += mlen
		for scsc := scan; scan < len(new); scan++ {
			pos, mlen = ix.Find(new, scan, scan+lastoffset)
			for ; scsc < scan+mlen; scsc++ {
				if inOld(scsc+lastoffset) && old[scsc+lastoffset] == new[scsc] {
					oldscore++
				}
			}
			// stop when a fresh match explains the bytes ahead better than
			// carrying on at the previous offset does; the margin is what a
			// control triple costs
			if mlen == oldscore && mlen != 0 || mlen > oldscore+8 {
				break
			}
			if inOld(scan+lastoffset) && old[scan+lastoffset] == new[scan] {
				oldscore--
			}
		}
		if mlen != oldscore || scan == len(new) {
			// extend the previous match forward and this one backward while
			// each added byte pays for itself: two matching bytes for every
			// byte of difference stream
			var s, best, lenf int
			for i := 0; lastscan+i < scan && lastpos+i < len(old); i++ {
				if old[lastpos+i] == new[lastscan+i] {
					s++
				}
				if s*2-(i+1) > best*2-lenf {
					best, lenf = s, i+1
				}
			}
			lenb := 0
			if scan < len(new) {
				s, best = 0, 0
				for i := 1; scan >= lastscan+i && pos >= i; i++ {
					if old[pos-i] == new[scan-i] {
						s++
					}
					if s*2-i > best*2-lenb {
						best, lenb = s, i
					}
				}
			}
			if lastscan+lenf > scan-lenb {
				// the two extensions overlap: split the overlap where it
				// stops helping the first and starts helping the second
				overlap := lastscan + lenf - (scan - lenb)
				s, best, lens := 0, 0, 0
				for i := 0; i < overlap; i++ {
					if new[lastscan+lenf-overlap+i] == old[lastpos+lenf-overlap+i] {
						s++
					}
					if new[scan-lenb+i] == old[pos-lenb+i] {
						s--
					}
					if s > best {
						best, lens = s, i+1
					}
				}
				lenf += lens - overlap
				lenb -= lens
			}
			d.write(new[lastscan:lastscan+lenf], old[lastpos:lastpos+lenf])
			nextra := (scan - lenb) - (lastscan + lenf)
			extra.raw(new[lastscan+lenf : lastscan+lenf+nextra])
			ctrl.u(uint64(lenf))
			ctrl.u(uint64(nextra))
			ctrl.s(int64((pos - lenb) - (lastpos + lenf)))
			ntriples++
			lastscan, lastpos, lastoffset = scan-lenb, pos-lenb, pos-scan
		}
	}
	return packPlain(ntriples, &ctrl, &diff, &extra)
}

// diffWriter turns the byte-wise difference of matched regions into runs of
// non-zero bytes: (gap since the last run, length, bytes). Positions are in
// one logical space running across all triples, so the decoder walks it once.
type diffWriter struct {
	w       *wbuf
	logical int64 // difference bytes seen so far
	lastEnd int64 // logical end of the last run written
}

func (d *diffWriter) write(new, old []byte) {
	for i := 0; i < len(new); {
		if new[i] == old[i] {
			i++
			continue
		}
		s := i
		e := i + 1
		for e < len(new) {
			k := e
			for k < len(new) && k-e < diffMergeGap && new[k] == old[k] {
				k++
			}
			if k-e < diffMergeGap && k < len(new) {
				e = k + 1
				continue
			}
			break
		}
		d.w.u(uint64(d.logical + int64(s) - d.lastEnd))
		d.w.u(uint64(e - s))
		for k := s; k < e; k++ {
			d.w.b = append(d.w.b, new[k]-old[k])
		}
		d.lastEnd = d.logical + int64(e)
		i = e
	}
	d.logical += int64(len(new))
}

// diffReader replays diffWriter's runs onto a region already copied from the
// old file.
type diffReader struct {
	r       rbuf
	logical int64
	next    int64  // logical start of the pending run
	pend    []byte // its bytes
	max     int64
}

func newDiffReader(b []byte, max int64) *diffReader {
	d := &diffReader{r: rbuf{b: b}, max: max}
	d.advance()
	return d
}

func (d *diffReader) advance() {
	if len(d.r.b) == 0 {
		d.pend, d.next = nil, 1<<62
		return
	}
	gap := d.r.un(uint64(d.max), "difference run gap")
	n := d.r.un(uint64(d.max), "difference run length")
	d.next += int64(gap)
	d.pend = d.r.take(n)
	if d.r.err != nil {
		d.pend, d.next = nil, 1<<62
	}
}

// add applies the difference bytes covering the next len(dst) logical bytes.
func (d *diffReader) add(dst []byte) error {
	end := d.logical + int64(len(dst))
	for d.next < end {
		if d.r.err != nil {
			return d.r.err
		}
		if d.next < d.logical {
			return fmt.Errorf("%w: difference run at %d is behind the cursor %d", errCorrupt, d.next, d.logical)
		}
		n := min(int64(len(d.pend)), end-d.next)
		base := d.next - d.logical
		for i := int64(0); i < n; i++ {
			dst[base+i] += d.pend[i]
		}
		if n < int64(len(d.pend)) {
			// the run straddles the end of this region; keep the rest
			d.pend = d.pend[n:]
			d.next += n
			break
		}
		d.next += int64(len(d.pend))
		d.advance()
	}
	d.logical = end
	return nil
}

func (d *diffReader) done() error {
	if d.r.err != nil {
		return d.r.err
	}
	if len(d.r.b) != 0 || len(d.pend) != 0 {
		return fmt.Errorf("%w: difference stream has bytes after the last region", errCorrupt)
	}
	return nil
}

func packPlain(ntriples int, ctrl, diff, extra *wbuf) []byte {
	w := &wbuf{}
	w.u(uint64(ntriples))
	w.u(uint64(len(ctrl.b)))
	w.u(uint64(len(diff.b)))
	w.u(uint64(len(extra.b)))
	w.raw(ctrl.b)
	w.raw(diff.b)
	w.raw(extra.b)
	return w.b
}

func applyPlain(old, body []byte, h *Header) ([]byte, error) {
	return plainPatch(old, body, h.NewSize)
}

// plainPatch replays plainDiff. newLen comes from the container, never from
// the body, so a corrupt patch cannot ask for an arbitrary allocation.
func plainPatch(old, body []byte, newLen int64) ([]byte, error) {
	r := &rbuf{b: body}
	ntriples := r.un(uint64(len(body)), "triple count")
	ctrlLen := r.un(uint64(len(body)), "control stream length")
	diffLen := r.un(uint64(len(body)), "difference stream length")
	extraLen := r.un(uint64(newLen), "literal stream length")
	ctrl := &rbuf{b: r.take(ctrlLen)}
	diff := r.take(diffLen)
	extra := r.take(extraLen)
	if err := r.done(); err != nil {
		return nil, err
	}
	out := make([]byte, newLen)
	d := newDiffReader(diff, newLen)
	var opos, npos int64
	for i := uint64(0); i < ntriples; i++ {
		nd := int64(ctrl.un(uint64(newLen), "difference length"))
		ne := int64(ctrl.un(uint64(newLen), "literal length"))
		seek := ctrl.s()
		if ctrl.err != nil {
			return nil, ctrl.err
		}
		if npos+nd > int64(len(out)) || opos < 0 || opos+nd > int64(len(old)) {
			return nil, fmt.Errorf("%w: triple %d: %d matched bytes at old %d / new %d", errCorrupt, i, nd, opos, npos)
		}
		copy(out[npos:npos+nd], old[opos:opos+nd])
		if err := d.add(out[npos : npos+nd]); err != nil {
			return nil, err
		}
		npos, opos = npos+nd, opos+nd
		if ne > int64(len(extra)) || npos+ne > int64(len(out)) {
			return nil, fmt.Errorf("%w: triple %d: %d literal bytes at new %d", errCorrupt, i, ne, npos)
		}
		copy(out[npos:], extra[:ne])
		extra = extra[ne:]
		npos += ne
		opos += seek
	}
	if err := ctrl.done(); err != nil {
		return nil, err
	}
	if err := d.done(); err != nil {
		return nil, err
	}
	if npos != int64(len(out)) || len(extra) != 0 {
		return nil, fmt.Errorf("%w: plain patch produced %d of %d bytes", errCorrupt, npos, len(out))
	}
	return out, nil
}
