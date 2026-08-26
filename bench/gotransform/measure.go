package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

var (
	hdiffz   = "/home/will/Code/binsync/bench/out/tools/HDiffPatch/hdiffz"
	hpatchz  = "/home/will/Code/binsync/bench/out/tools/HDiffPatch/hpatchz"
	scratch  = envOr("GOTRANSFORM_TMP", "/tmp/claude-1000/-home-will-Code-binsync/49262ac6-3517-4d5a-b8ec-a8634d869b18/scratchpad/gt")
	toolsSem = make(chan struct{}, 20)
	// hdiffzThreads is the -p-N of every hdiffz run; the 06-* logs used 1,
	// the 08-gt127-* logs 8 (the patch bytes do not depend on it).
	hdiffzThreads = envOr("GOTRANSFORM_HDIFFZ_THREADS", "8")
)

func hdiffzArgs(old, new, patch string) []string {
	return []string{"-m-6", "-SD", "-d", "-f", "-p-" + hdiffzThreads, "-c-zstd-21-24", old, new, patch}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Delta is the result of measuring old->new with several encoders.
type Delta struct {
	Bsdiff, Hdiffz, Zstd    int64
	TBsdiff, THdiffz, TZstd time.Duration
	// raw byte comparison at the same section-relative offset
	Diff, Runs     int64
	OldLen, NewLen int64
}

func (d Delta) String() string {
	return fmt.Sprintf("bsdiff=%d hdiffz=%d zstd=%d rawdiff=%d runs=%d", d.Bsdiff, d.Hdiffz, d.Zstd, d.Diff, d.Runs)
}

// rawDiff counts differing bytes at equal offsets and byte-exact runs.
func rawDiff(a, b []byte) (diff, runs int64) {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	in := false
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			diff++
			if !in {
				runs++
				in = true
			}
		} else {
			in = false
		}
	}
	if len(a) != len(b) {
		diff += int64(abs(len(a) - len(b)))
		runs++
	}
	return
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// measure runs bsdiff, hdiffz and zstd --patch-from on old->new and returns
// patch sizes. Empty inputs get an empty-file treatment.
func measure(tag string, old, new []byte, which string) Delta {
	if which == "" {
		which = "bhz"
	}
	var d Delta
	d.OldLen, d.NewLen = int64(len(old)), int64(len(new))
	d.Diff, d.Runs = rawDiff(old, new)
	os.MkdirAll(scratch, 0o755)
	base := filepath.Join(scratch, sanitize(tag))
	of, nf := base+".old", base+".new"
	must(os.WriteFile(of, old, 0o644))
	must(os.WriteFile(nf, new, 0o644))
	var wg sync.WaitGroup
	run := func(f func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			toolsSem <- struct{}{}
			defer func() { <-toolsSem }()
			f()
		}()
	}
	if bytes.ContainsRune([]byte(which), 'b') {
		run(func() {
			p := base + ".bsdiff"
			t := time.Now()
			d.Bsdiff = runTool(p, "bsdiff", of, nf, p)
			d.TBsdiff = time.Since(t)
		})
	}
	if bytes.ContainsRune([]byte(which), 'h') {
		run(func() {
			p := base + ".hdiff"
			t := time.Now()
			d.Hdiffz = runTool(p, hdiffz, hdiffzArgs(of, nf, p)...)
			d.THdiffz = time.Since(t)
		})
	}
	if bytes.ContainsRune([]byte(which), 'z') {
		run(func() {
			p := base + ".zst"
			t := time.Now()
			d.Zstd = runTool(p, "zstd", "-q", "-f", "-19", "--long=27", "--patch-from="+of, nf, "-o", p)
			d.TZstd = time.Since(t)
		})
	}
	wg.Wait()
	return d
}

func runTool(out string, cmd string, args ...string) int64 {
	os.Remove(out)
	c := exec.Command(cmd, args...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: %s %v: %v: %s\n", cmd, args, err, stderr.String())
		return -1
	}
	st, err := os.Stat(out)
	if err != nil {
		return -1
	}
	return st.Size()
}

// verifyBspatch applies the bsdiff patch produced by measure(tag) and checks
// the result equals new.
func verifyBspatch(tag string, new []byte) bool {
	base := filepath.Join(scratch, sanitize(tag))
	out := base + ".bsout"
	c := exec.Command("bspatch", base+".old", out, base+".bsdiff")
	if err := c.Run(); err != nil {
		return false
	}
	got, err := os.ReadFile(out)
	return err == nil && bytes.Equal(got, new)
}

func sanitize(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			b[i] = '_'
		}
	}
	return string(b)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// zstdSize compresses data alone with zstd -19 and returns the size.
func zstdSize(tag string, data []byte) int64 {
	os.MkdirAll(scratch, 0o755)
	base := filepath.Join(scratch, sanitize(tag))
	must(os.WriteFile(base+".raw", data, 0o644))
	return runTool(base+".raw.zst", "zstd", "-q", "-f", "-19", base+".raw", "-o", base+".raw.zst")
}
