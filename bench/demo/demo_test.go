package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"binsync/delta"
	"binsync/release"
)

// buildAssets writes the smallest possible version of what
// bench/demo/build-assets.sh produces: one pair, one real patch, one real
// pointer. The bytes are not Go binaries, so the patch is the plain codec --
// which is the point, the server does not care which transform it serves.
func buildAssets(t *testing.T) (string, *pair) {
	t.Helper()
	dir := t.TempDir()
	id := "one-line"
	old := make([]byte, 40000)
	for i := range old {
		old[i] = byte(i*7 + i/251)
	}
	next := append(append([]byte{}, old[:20000]...), append([]byte("inserted"), old[20000:]...)...)

	patch, err := delta.Encode(old, next, delta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	oldH, newH := release.HashBytes(old), release.HashBytes(next)
	blobObj, blob := release.EncodeBlob(newH, next)
	ptr := &release.Pointer{
		Format: release.Format, Seq: 1,
		Head:  release.Release{Hash: newH, Size: int64(len(next)), Blob: blob},
		Chain: []release.Edge{{From: oldH, To: newH, Key: release.PatchKey(oldH, newH), Size: int64(len(patch)), B3: release.HashBytes(patch)}},
	}
	ptrBytes, err := ptr.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	p := &pair{
		ID: id, Title: "One-line change", Blurb: "b",
		OldSize: int64(len(old)), NewSize: int64(len(next)),
		PatchKey: ptr.Chain[0].Key, PatchSize: int64(len(patch)),
		HdiffSize: 999, BlobKey: blob.Key, BlobSize: blob.Size,
		FromHash: oldH.String(), ToHash: newH.String(),
	}
	meta, _ := json.Marshal(p)
	for path, b := range map[string][]byte{
		"old.bin":                     old,
		"meta.json":                   meta,
		"compare/patch.hdiff":         []byte("not a real hdiff"),
		"store/" + release.PointerKey: ptrBytes,
		"store/" + ptr.Chain[0].Key:   patch,
		"store/" + blob.Key:           blobObj,
	} {
		full := filepath.Join(dir, id, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, p
}

func newServer(t *testing.T, dir string) http.Handler {
	t.Helper()
	pairs, err := loadPairs(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{
		pairs: pairs, dir: dir, machine: "m", region: "dev",
		limit: newLimiter(), apply: make(chan struct{}, 1), log: testLogger(t),
	}
	return s.routes()
}

func get(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w
}

func TestServesTheRealObjects(t *testing.T) {
	t.Parallel()
	dir, p := buildAssets(t)
	h := newServer(t, dir)

	for _, tc := range []struct {
		target string
		want   int
	}{
		{"/", 200},
		{"/healthz", 200},
		{"/api/pairs", 200},
		{"/s/one-line/latest.json", 200},
		{"/s/one-line/" + p.PatchKey, 200},
		{"/s/one-line/" + p.BlobKey, 200},
		{"/s/one-line/compare/patch.hdiff", 200},
		{"/s/one-line/blobs/other.blob", 404},
		{"/s/one-line/old.bin", 404},   // the old binary is not published
		{"/s/one-line/meta.json", 404}, // nor is the metadata a store object
		{"/s/nope/latest.json", 404},
	} {
		if w := get(t, h, "GET", tc.target); w.Code != tc.want {
			t.Errorf("GET %s = %d, want %d", tc.target, w.Code, tc.want)
		}
	}

	w := get(t, h, "GET", "/s/one-line/latest.json")
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store: a cached run measures nothing", got)
	}
	if got := w.Header().Get("X-Served-By"); got != "m dev" {
		t.Errorf("X-Served-By = %q", got)
	}
	if _, err := release.ParsePointer(w.Body.Bytes()); err != nil {
		t.Errorf("the pointer the page fetches does not parse: %v", err)
	}
}

// TestApplyIsTheRealThing is the demo's whole claim: the patch the page
// downloads, applied to the old binary the machine holds, hashes to the head
// the pointer names.
func TestApplyIsTheRealThing(t *testing.T) {
	t.Parallel()
	dir, p := buildAssets(t)
	h := newServer(t, dir)

	w := get(t, h, "POST", "/api/apply?pair=one-line")
	if w.Code != 200 {
		t.Fatalf("apply = %d: %s", w.Code, w.Body)
	}
	var res applyResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Verified || res.Hash != p.ToHash {
		t.Fatalf("apply produced %s, want %s (verified=%v)", res.Hash, p.ToHash, res.Verified)
	}
	if res.Size != p.NewSize {
		t.Errorf("apply produced %d bytes, want %d", res.Size, p.NewSize)
	}
	if res.ApplyMS <= 0 {
		t.Errorf("apply_ms = %v", res.ApplyMS)
	}
	if w := get(t, h, "POST", "/api/apply?pair=nope"); w.Code != 404 {
		t.Errorf("apply of an unknown pair = %d, want 404", w.Code)
	}
}

// TestBigObjectsAreRateLimited: the two comparison downloads are the demo's
// egress bill, and the small objects the page always fetches must never be
// caught by the cap.
func TestBigObjectsAreRateLimited(t *testing.T) {
	t.Parallel()
	dir, p := buildAssets(t)
	h := newServer(t, dir)

	n := 0
	for range 12 {
		if get(t, h, "GET", "/s/one-line/"+p.BlobKey).Code == 200 {
			n++
		}
	}
	if n != 10 {
		t.Errorf("%d blob downloads allowed, want 10", n)
	}
	for range 12 {
		for _, k := range []string{"latest.json", p.PatchKey} {
			if w := get(t, h, "GET", "/s/one-line/"+k); w.Code != 200 {
				t.Fatalf("GET %s = %d: the update path must not be rate-limited", k, w.Code)
			}
		}
	}
}

func TestLimiterRefills(t *testing.T) {
	t.Parallel()
	l := newLimiter()
	for range 10 {
		if !l.allow("a") {
			t.Fatal("the burst is 10")
		}
	}
	if l.allow("a") {
		t.Fatal("the 11th should be refused")
	}
	if !l.allow("b") {
		t.Fatal("a different client has its own bucket")
	}
	l.seen["a"].last = time.Now().Add(-7 * time.Minute)
	if !l.allow("a") {
		t.Fatal("one token should have refilled after six minutes")
	}
}

func TestMissingAssetsIsAStartupError(t *testing.T) {
	t.Parallel()
	if _, err := loadPairs(t.TempDir()); err == nil {
		t.Fatal("an empty asset directory must not start")
	}
	dir, _ := buildAssets(t)
	if err := os.Remove(filepath.Join(dir, "one-line", "old.bin")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPairs(dir); err == nil || !strings.Contains(err.Error(), "old.bin") {
		t.Fatalf("a missing asset must name itself, got %v", err)
	}
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(b)))
	return len(b), nil
}
