package main

import (
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"time"
)

// Patch file layout (all sizes uvarint):
//
//	"GTP2" s1kind s2kind newLen sha256(old) sha256(new) sha256(pred)
//	len layout(zstd) | len stage1a | len stage1b | len stage2
//
// s1kind: 'h' hdiffz, 'b' bsdiff, 'z' zstd --patch-from; stage 1a is old
// funcnametab+filetab -> new, stage 1b is the predicted cutab+pctab+
// go:func.* (predictBlobs) -> new.
// s2kind: 'p' positional (zstd of gap/len/bytes runs), or h/b/z as above
// (predicted file -> new file).
const patchMagic = "GTP2"

type patchFile struct {
	S1Kind, S2Kind byte
	NewLen         uint64
	OldSum, NewSum [32]byte
	PredSum        [32]byte
	Layout         []byte
	S1a, S1b, S2   []byte
}

func (p *patchFile) marshal() []byte {
	w := &wbuf{}
	w.b = append(w.b, patchMagic...)
	w.b = append(w.b, p.S1Kind, p.S2Kind)
	w.u(p.NewLen)
	w.b = append(w.b, p.OldSum[:]...)
	w.b = append(w.b, p.NewSum[:]...)
	w.b = append(w.b, p.PredSum[:]...)
	w.bytes(p.Layout)
	w.bytes(p.S1a)
	w.bytes(p.S1b)
	w.bytes(p.S2)
	return w.b
}

func parsePatch(b []byte) (*patchFile, error) {
	if len(b) < 6 || string(b[:4]) != patchMagic {
		return nil, fmt.Errorf("bad patch magic")
	}
	p := &patchFile{S1Kind: b[4], S2Kind: b[5]}
	r := &rbuf{b: b[6:]}
	p.NewLen = r.u()
	take := func(dst *[32]byte) {
		if len(r.b) < 32 {
			r.err = fmt.Errorf("short patch header")
			return
		}
		copy(dst[:], r.b[:32])
		r.b = r.b[32:]
	}
	take(&p.OldSum)
	take(&p.NewSum)
	take(&p.PredSum)
	p.Layout = r.bytes()
	p.S1a = r.bytes()
	p.S1b = r.bytes()
	p.S2 = r.bytes()
	return p, r.err
}

// ---- positional stage 2 ----------------------------------------------------

// encodePositional encodes new as runs of bytes that differ from pred at the
// same offset: uvarint(total length), then per run uvarint(gap since the end
// of the previous run) uvarint(len) bytes. Runs closer than mergeGap bytes
// are merged (a run header costs about as much as the literals between).
const mergeGap = 4

func encodePositional(pred, new []byte) []byte {
	w := &wbuf{}
	w.u(uint64(len(new)))
	at := func(i int) byte {
		if i < len(pred) {
			return pred[i]
		}
		return 0
	}
	last := 0 // end of the previous run
	i := 0
	for i < len(new) {
		if at(i) == new[i] {
			i++
			continue
		}
		start := i
		end := i + 1
		for end < len(new) {
			// extend over equal bytes if another difference follows within mergeGap
			k := end
			for k < len(new) && k-end < mergeGap && at(k) == new[k] {
				k++
			}
			if k < len(new) && k-end < mergeGap && at(k) != new[k] {
				end = k + 1
				continue
			}
			break
		}
		w.u(uint64(start - last))
		w.u(uint64(end - start))
		w.b = append(w.b, new[start:end]...)
		last = end
		i = end
	}
	return w.b
}

func applyPositional(pred, patch []byte) ([]byte, error) {
	r := &rbuf{b: patch}
	n := int(r.u())
	out := make([]byte, n)
	copy(out, pred)
	pos := 0
	for len(r.b) > 0 && r.err == nil {
		pos += int(r.u())
		l := int(r.u())
		if r.err != nil || pos+l > n || l > len(r.b) {
			return nil, fmt.Errorf("positional patch corrupt")
		}
		copy(out[pos:], r.b[:l])
		r.b = r.b[l:]
		pos += l
	}
	return out, r.err
}

// ---- external tools --------------------------------------------------------

func tmpPath(tag string) string {
	os.MkdirAll(scratch, 0o755)
	return filepath.Join(scratch, sanitize(tag))
}

func runCmd(name string, args ...string) error {
	c := exec.Command(name, args...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("%s %v: %v: %s", name, args, err, stderr.String())
	}
	return nil
}

// zstdC / zstdD compress and decompress a standalone blob with the zstd CLI.
func zstdC(tag string, data []byte) []byte {
	base := tmpPath(tag)
	must(os.WriteFile(base+".raw", data, 0o644))
	must(runCmd("zstd", "-q", "-f", "-19", base+".raw", "-o", base+".raw.zst"))
	out, err := os.ReadFile(base + ".raw.zst")
	must(err)
	return out
}

func zstdD(tag string, data []byte) ([]byte, error) {
	base := tmpPath(tag)
	if err := os.WriteFile(base+".zst", data, 0o644); err != nil {
		return nil, err
	}
	if err := runCmd("zstd", "-q", "-f", "-d", base+".zst", "-o", base+".dec"); err != nil {
		return nil, err
	}
	return os.ReadFile(base + ".dec")
}

// toolDelta produces a generic delta old -> new with the given encoder.
func toolDelta(kind byte, tag string, old, new []byte) ([]byte, error) {
	base := tmpPath(tag)
	of, nf, pf := base+".old", base+".new", base+".patch"
	must(os.WriteFile(of, old, 0o644))
	must(os.WriteFile(nf, new, 0o644))
	var err error
	switch kind {
	case 'b':
		err = runCmd("bsdiff", of, nf, pf)
	case 'h':
		err = runCmd(hdiffz, hdiffzArgs(of, nf, pf)...)
	case 'z':
		err = runCmd("zstd", "-q", "-f", "-19", "--long=27", "--patch-from="+of, nf, "-o", pf)
	default:
		return nil, fmt.Errorf("unknown delta kind %q", kind)
	}
	if err != nil {
		return nil, err
	}
	return os.ReadFile(pf)
}

// toolApply applies a generic delta.
func toolApply(kind byte, tag string, old, patch []byte) ([]byte, error) {
	base := tmpPath(tag)
	of, nf, pf := base+".old", base+".out", base+".patch"
	must(os.WriteFile(of, old, 0o644))
	must(os.WriteFile(pf, patch, 0o644))
	var err error
	switch kind {
	case 'b':
		err = runCmd("bspatch", of, nf, pf)
	case 'h':
		err = runCmd(hpatchz, "-f", of, pf, nf)
	case 'z':
		err = runCmd("zstd", "-q", "-f", "-d", "--long=27", "--patch-from="+of, pf, "-o", nf)
	default:
		return nil, fmt.Errorf("unknown delta kind %q", kind)
	}
	if err != nil {
		return nil, err
	}
	return os.ReadFile(nf)
}

// ---- encode ----------------------------------------------------------------

func cmdEncode(args []string) {
	fs := flag.NewFlagSet("encode", flag.ExitOnError)
	s1 := fs.String("s1", "h", "stage-1 encoder: h=hdiffz b=bsdiff z=zstd")
	s2 := fs.String("s2", "p", "stage-2 encoder: p=positional h=hdiffz b=bsdiff z=zstd")
	block := fs.Int("block", 16, "data-map block size")
	stats := fs.Bool("stats", true, "print the stage-2 residual by section")
	dumpPred := fs.String("dumppred", "", "write the predicted file here (debugging)")
	typeResid := fs.Int("typeresid", 0, "classify the type-section residual and print up to N runs (debugging)")
	fs.BoolVar(&typeRewrite, "types", true, "rewrite type-descriptor nameOff/typeOff/textOff fields")
	fs.IntVar(&dataTol, "datatol", 0, "fuzzy tolerance for data-section maps")
	cpuprof := fs.String("cpuprofile", "", "write a CPU profile of the encoder here")
	fs.IntVar(&sampleBits, "samplebits", sampleBits, "content-map index sampling: 1 in 2^n windows")
	fs.IntVar(&lookahead, "lookahead", lookahead, "content-map lookup window range beyond the block")
	fs.IntVar(&maxChain, "maxchain", maxChain, "content-map: skip windows with this many occurrences")
	fs.IntVar(&ovMinVotes, "ovmin", ovMinVotes, "target override: minimum agreeing references")
	fs.IntVar(&ovRounds, "ovrounds", ovRounds, "target vote / block-fix rounds")
	fs.Parse(args)
	if fs.NArg() < 3 {
		fatal("usage: encode [-s1 h|b|z] [-s2 p|h|b|z] OLD NEW PATCH")
	}
	if *cpuprof != "" {
		pf, err := os.Create(*cpuprof)
		must(err)
		must(pprof.StartCPUProfile(pf))
		defer pprof.StopCPUProfile()
	}
	t0 := time.Now()
	lap := func(what string) {
		fmt.Printf("  [%6.2fs] %s\n", time.Since(t0).Seconds(), what)
	}
	old, err := loadBin(fs.Arg(0))
	must(err)
	new, err := loadBin(fs.Arg(1))
	must(err)
	if !old.Pcln.Inside || !new.Pcln.Inside {
		fatal("encode: only Go 1.26/1.27 layouts (go:func.* inside .gopclntab) are supported; got %s -> %s", old.Layout, new.Layout)
	}
	lap(fmt.Sprintf("loaded %s (%s, %d B) -> %s (%s, %d B)", baseName(old.Path), old.Layout, len(old.File), baseName(new.Path), new.Layout, len(new.File)))
	tag := "enc-" + baseName(old.Path) + "-" + baseName(new.Path)
	m := matchFuncs(old, new)
	lap(fmt.Sprintf("match: exact=%d normalised=%d content=%d unmatched-new=%d unmatched-old=%d", m.Exact, m.Norm, m.Content, m.Unmatched, m.UnmatchedOld))
	dmaps, shifts := buildMaps(old, new, m, *block, lap)
	for _, n := range dataMapSects {
		if dm := dmaps[n]; dm != nil {
			fmt.Printf("  datamap %s: %s; rle %d B\n", n, dm, len(dm.EncodeRLE()))
		}
	}
	for _, n := range []string{".bss", ".noptrbss"} {
		if st := shifts[n]; st != nil {
			fmt.Printf("  shift table %s: %s\n", n, st)
		}
	}
	lap("content maps + shift tables")
	overrides, ost := deriveOverrides(old, new, m, dmaps, shifts)
	lap(fmt.Sprintf("overrides: %s", ost))
	lay := buildLayout(old, new, m, dmaps, shifts, overrides)
	layRaw := lay.Encode(old)
	layZ := zstdC(tag+"-layout", layRaw)
	lap(fmt.Sprintf("layout: %d funcs, %d rec shapes, raw %d B, zstd %d B", lay.NFunc, len(lay.RecShapes), len(layRaw), len(layZ)))

	// the prediction is computed exactly as the decoder will compute it:
	// from the decoded layout and the stage-1 output, not from `new`
	lay2, err := DecodeLayout(layRaw, old)
	must(err)
	skel, m2, err := skeletonBin(old, lay2)
	must(err)
	s1aOld, s1aNew := stage1aBlobs(old), stage1aBlobs(new)
	s1a, err := toolDelta((*s1)[0], tag+"-s1a", s1aOld, s1aNew)
	must(err)
	must(fillSkeleton(skel, skel.Pcln.ranges1a(), s1aNew))
	lap(fmt.Sprintf("stage 1a (%c): funcnametab+filetab %d -> %d B, delta %d B", (*s1)[0], len(s1aOld), len(s1aNew), len(s1a)))
	mp := &mapper{src: old, dst: skel, srcToDst: m2.OldToNew, dataMaps: lay2.DataMaps, shiftTabs: lay2.Shifts, overrides: overrideMap(lay2.Overrides)}
	bp := predictBlobs(old, skel, m2, mp)
	s1bPred, s1bNew := bp.concat(), stage1bBlobs(new)
	s1b, err := toolDelta((*s1)[0], tag+"-s1b", s1bPred, s1bNew)
	must(err)
	if d := os.Getenv("GOTRANSFORM_DUMP"); d != "" { // per-table analysis of the stage-1b residual
		o := uint64(0)
		for i, r := range skel.Pcln.ranges1b() {
			must(os.WriteFile(fmt.Sprintf("%s/s1b-%d.pred", d, i), s1bPred[o:o+r.Len], 0o644))
			must(os.WriteFile(fmt.Sprintf("%s/s1b-%d.new", d, i), s1bNew[o:o+r.Len], 0o644))
			o += r.Len
		}
	}
	must(fillSkeleton(skel, skel.Pcln.ranges1b(), s1bNew))
	mp.blobs = bp
	lap(fmt.Sprintf("stage 1b (%c): cutab+pctab+go:func.* predicted %d -> %d B, delta %d B; %s", (*s1)[0], len(s1bPred), len(s1bNew), len(s1b), bp.Stats))
	pred, st, ts := predictWholeStats(old, skel, lay2, m2, mp)
	d, r := rawDiff(pred, new.File)
	lap(fmt.Sprintf("prediction: %d B (new %d B), insns=%d decode-failures=%d, differing bytes=%d in %d runs", len(pred), len(new.File), st.Insns, st.Fails, d, r))
	if typeRewrite {
		fmt.Printf("  type descriptors: %s\n", ts)
	}
	if *stats {
		fmt.Printf("  ")
		printResidual(pred, new)
	}
	if *dumpPred != "" {
		must(os.WriteFile(*dumpPred, pred, 0o644))
	}
	if *typeResid > 0 {
		typeResidual(old, new, lay2.DataMaps, pred, *typeResid)
	}

	var s2b []byte
	if (*s2)[0] == 'p' {
		raw := encodePositional(pred, new.File)
		s2b = zstdC(tag+"-s2", raw)
		lap(fmt.Sprintf("stage 2 (positional): raw %d B, zstd %d B", len(raw), len(s2b)))
	} else {
		s2b, err = toolDelta((*s2)[0], tag+"-s2", pred, new.File)
		must(err)
		lap(fmt.Sprintf("stage 2 (%c): %d B", (*s2)[0], len(s2b)))
	}
	pf := &patchFile{S1Kind: (*s1)[0], S2Kind: (*s2)[0], NewLen: uint64(len(new.File)), OldSum: sha256.Sum256(old.File), NewSum: sha256.Sum256(new.File), PredSum: sha256.Sum256(pred), Layout: layZ, S1a: s1a, S1b: s1b, S2: s2b}
	out := pf.marshal()
	must(os.WriteFile(fs.Arg(2), out, 0o644))
	hdr := len(out) - len(layZ) - len(s1a) - len(s1b) - len(s2b)
	fmt.Printf("PATCH %s: %d B = header %d + layout %d + stage1(%c) %d (1a %d + 1b %d) + stage2(%c) %d  [encode %.2fs]\n", fs.Arg(2), len(out), hdr, len(layZ), pf.S1Kind, len(s1a)+len(s1b), len(s1a), len(s1b), pf.S2Kind, len(s2b), time.Since(t0).Seconds())
}

// ---- decode ----------------------------------------------------------------

func cmdDecode(args []string) {
	fs := flag.NewFlagSet("decode", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() < 3 {
		fatal("usage: decode OLD PATCH NEW")
	}
	t0 := time.Now()
	lap := func(what string) {
		fmt.Printf("  [%6.2fs] %s\n", time.Since(t0).Seconds(), what)
	}
	old, err := loadBin(fs.Arg(0))
	must(err)
	praw, err := os.ReadFile(fs.Arg(1))
	must(err)
	pf, err := parsePatch(praw)
	must(err)
	if sha256.Sum256(old.File) != pf.OldSum {
		fatal("decode: old file hash mismatch")
	}
	tag := "dec-" + baseName(old.Path)
	layRaw, err := zstdD(tag+"-layout", pf.Layout)
	must(err)
	lay, err := DecodeLayout(layRaw, old)
	must(err)
	lap(fmt.Sprintf("loaded old (%s) and patch: %d B, layout %d B", old.Layout, len(praw), len(layRaw)))
	skel, m, err := skeletonBin(old, lay)
	must(err)
	s1aNew, err := toolApply(pf.S1Kind, tag+"-s1a", stage1aBlobs(old), pf.S1a)
	must(err)
	must(fillSkeleton(skel, skel.Pcln.ranges1a(), s1aNew))
	mp := &mapper{src: old, dst: skel, srcToDst: m.OldToNew, dataMaps: lay.DataMaps, shiftTabs: lay.Shifts, overrides: overrideMap(lay.Overrides)}
	bp := predictBlobs(old, skel, m, mp)
	s1bNew, err := toolApply(pf.S1Kind, tag+"-s1b", bp.concat(), pf.S1b)
	must(err)
	must(fillSkeleton(skel, skel.Pcln.ranges1b(), s1bNew))
	mp.blobs = bp
	lap(fmt.Sprintf("stage 1 applied: %d + %d B of pclntab tables; skeleton %d funcs", len(s1aNew), len(s1bNew), len(skel.Funcs)))
	pred, _ := predictWhole(old, skel, lay, m, mp)
	if sha256.Sum256(pred) != pf.PredSum {
		fatal("decode: predicted base hash mismatch (encoder/decoder divergence)")
	}
	lap(fmt.Sprintf("prediction: %d B, hash verified", len(pred)))
	var out []byte
	if pf.S2Kind == 'p' {
		raw, err := zstdD(tag+"-s2", pf.S2)
		must(err)
		out, err = applyPositional(pred, raw)
		must(err)
	} else {
		out, err = toolApply(pf.S2Kind, tag+"-s2", pred, pf.S2)
		must(err)
	}
	if uint64(len(out)) != pf.NewLen || sha256.Sum256(out) != pf.NewSum {
		fatal("decode: output hash mismatch (%d B)", len(out))
	}
	must(os.WriteFile(fs.Arg(2), out, 0o755))
	lap(fmt.Sprintf("stage 2 (%c) applied: %d B written to %s, hash verified", pf.S2Kind, len(out), fs.Arg(2)))
	fmt.Printf("DECODE OK %s: %d B [decode %.2fs]\n", fs.Arg(2), len(out), time.Since(t0).Seconds())
}
