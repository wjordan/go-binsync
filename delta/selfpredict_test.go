package delta

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/wjordan/go-binsync/delta/gobin"
)

// The self-prediction gate (docs/DESIGN.md 3.6, D14). Encoding a binary
// against itself exercises every predictor -- the function layout, the
// pclntab rebuild, the stage-1b blob emulation, the type-descriptor walk,
// the instruction relocator -- with a known answer: the prediction must
// reproduce the input byte for byte, so the correction is empty.
//
// This is what qualifies a new Go release. It needs binaries built by that
// release, which the repository does not carry, so it runs over a corpus
// directory named by BINSYNC_CORPUS (bench/out/bin127 after bench/build.sh).
func TestSelfPrediction(t *testing.T) {
	for _, path := range corpus(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			t.Parallel()
			bin := readFile(t, path)
			if _, err := gobin.Parse(bin); err != nil {
				t.Skipf("not a binary the Go-aware codec takes: %v", err)
			}
			var st Stats
			patch, err := Encode(bin, bin, Options{Stats: &st})
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if st.Transform != TransformGoAMD64 {
				t.Fatalf("transform %d, want the Go-aware codec", st.Transform)
			}
			if st.PredictErr != 0 {
				t.Errorf("self-prediction differs from the input in %d bytes; "+
					"the codec does not reproduce this Go release's layout", st.PredictErr)
			}
			if st.Stage2 > 64 {
				t.Errorf("stage-2 correction is %d bytes, want an empty one", st.Stage2)
			}
			mustRoundTrip(t, bin, bin, patch)
		})
	}
}

// TestCorpusRoundTrip encodes every ordered pair of corpus binaries of the
// same size class and checks the patch reproduces the target exactly. It is
// the codec's end-to-end correctness test; the size numbers it prints are
// what the documentation quotes.
func TestCorpusRoundTrip(t *testing.T) {
	files := corpus(t)
	for i, a := range files {
		for j, b := range files {
			if i == j {
				continue
			}
			t.Run(filepath.Base(a)+"->"+filepath.Base(b), func(t *testing.T) {
				t.Parallel()
				old, new := readFile(t, a), readFile(t, b)
				var st Stats
				patch, err := Encode(old, new, Options{Stats: &st})
				if err != nil {
					t.Fatalf("Encode: %v", err)
				}
				mustRoundTrip(t, old, new, patch)
				t.Logf("transform %d, patch %d B (%.3f%% of %d), residual %d B",
					st.Transform, len(patch), 100*float64(len(patch))/float64(len(new)), len(new), st.PredictErr)
			})
		}
	}
}

func mustRoundTrip(t *testing.T, old, new, patch []byte) {
	t.Helper()
	var got bytes.Buffer
	got.Grow(len(new))
	if err := Apply(old, patch, &got); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !bytes.Equal(got.Bytes(), new) {
		t.Fatalf("Apply produced %d bytes, want %d, and they differ", got.Len(), len(new))
	}
}

// corpus lists the binaries BINSYNC_CORPUS names, skipping the test when it
// is unset.
func corpus(t *testing.T) []string {
	t.Helper()
	dir := os.Getenv("BINSYNC_CORPUS")
	if dir == "" {
		t.Skip("set BINSYNC_CORPUS to a directory of Go binaries to run the codec gate")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("BINSYNC_CORPUS: %v", err)
	}
	var out []string
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		// Stat, not e.Info: a corpus is often a directory of symlinks.
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		t.Fatalf("BINSYNC_CORPUS=%s holds no executables", dir)
	}
	return out
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
