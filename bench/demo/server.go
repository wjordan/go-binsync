package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"binsync/delta"
	"binsync/release"
)

type server struct {
	pairs           []*pair
	dir             string
	machine, region string
	limit           *limiter
	// apply is a semaphore of one: applying the prometheus pair peaks at
	// about 0.92 GB, and the Machine has 2 GB.
	apply chan struct{}
	log   *slog.Logger
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /api/pairs", s.apiPairs)
	mux.HandleFunc("POST /api/apply", s.apiApply)
	mux.HandleFunc("GET /s/{pair}/{key...}", s.object)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	return s.headers(mux)
}

// headers stamps every response with the Machine that served it and refuses
// to let anything be cached: a demo whose second run is served from the
// browser's disk cache measures nothing.
func (s *server) headers(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Served-By", s.machine+" "+s.region)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		h.ServeHTTP(w, r)
	})
}

func (s *server) page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func (s *server) apiPairs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"region":  s.region,
		"machine": s.machine,
		"pairs":   s.pairs,
	})
}

func (s *server) find(id string) *pair {
	for _, p := range s.pairs {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// object serves one store object, plus the one comparison object that is not
// in the store. Keys are matched against the pair's own metadata rather than
// resolved as paths, so the only reachable files are the four this pair
// published.
func (s *server) object(w http.ResponseWriter, r *http.Request) {
	p := s.find(r.PathValue("pair"))
	if p == nil {
		http.Error(w, "no such pair", http.StatusNotFound)
		return
	}
	key := r.PathValue("key")
	var file string
	var big bool
	switch key {
	case release.PointerKey:
		file = filepath.Join(s.dir, p.ID, "store", release.PointerKey)
	case p.PatchKey:
		file = filepath.Join(s.dir, p.ID, "store", filepath.FromSlash(p.PatchKey))
	case p.BlobKey:
		file, big = filepath.Join(s.dir, p.ID, "store", filepath.FromSlash(p.BlobKey)), true
	case "compare/patch.hdiff":
		file, big = filepath.Join(s.dir, p.ID, "compare", "patch.hdiff"), true
	default:
		http.Error(w, "no such object", http.StatusNotFound)
		return
	}
	// The two comparison downloads are the demo's whole egress bill, and
	// they are also the only thing here worth abusing.
	if big && !s.limit.allow(clientIP(r)) {
		w.Header().Set("Retry-After", "600")
		http.Error(w, "rate limit: the multi-megabyte comparison downloads are capped per client (docs/DEMO.md 3)", http.StatusTooManyRequests)
		return
	}
	f, err := os.Open(file)
	if err != nil {
		http.Error(w, "no such object", http.StatusNotFound)
		return
	}
	defer f.Close()
	if strings.HasSuffix(key, ".json") {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	// ServeContent, not ServeFile: the name is the key, not a path, and
	// modtimes here are image build times, which are not interesting.
	http.ServeContent(w, r, path.Base(key), time.Time{}, f)
}

type applyResult struct {
	ApplyMS  float64 `json:"apply_ms"`
	Hash     string  `json:"hash"`
	Want     string  `json:"want"`
	Verified bool    `json:"verified"`
	Size     int64   `json:"size"`
	Queued   bool    `json:"queued"`
}

// apiApply is step 3: the Machine applies the patch the browser just watched
// arrive, against its own copy of the old release, and reports what came out.
// The result is hashed and dropped; nothing is written.
func (s *server) apiApply(w http.ResponseWriter, r *http.Request) {
	p := s.find(r.URL.Query().Get("pair"))
	if p == nil {
		http.Error(w, "no such pair", http.StatusNotFound)
		return
	}
	queued := false
	select {
	case s.apply <- struct{}{}:
	default:
		queued = true
		select {
		case s.apply <- struct{}{}:
		case <-r.Context().Done():
			return
		case <-time.After(30 * time.Second):
			http.Error(w, "another apply is still running", http.StatusServiceUnavailable)
			return
		}
	}
	// The apply's working set is 7.6x the binary (docs/DESIGN.md 11.3) and
	// this server is idle between visitors, so the pages go back to the
	// kernel as soon as the answer is written rather than being held for a
	// next request that may be an hour away.
	defer func() { <-s.apply; debug.FreeOSMemory() }()

	old, err := os.ReadFile(filepath.Join(s.dir, p.ID, "old.bin"))
	if err != nil {
		s.fail(w, "reading the old release", err)
		return
	}
	patch, err := os.ReadFile(filepath.Join(s.dir, p.ID, "store", filepath.FromSlash(p.PatchKey)))
	if err != nil {
		s.fail(w, "reading the patch", err)
		return
	}

	var buf bytes.Buffer
	buf.Grow(int(p.NewSize))
	start := time.Now()
	if err := delta.Apply(old, patch, &buf); err != nil {
		s.fail(w, "applying the patch", err)
		return
	}
	ms := float64(time.Since(start).Microseconds()) / 1000
	got := release.HashBytes(buf.Bytes()).String()
	res := applyResult{
		ApplyMS: ms, Hash: got, Want: p.ToHash,
		Verified: got == p.ToHash, Size: int64(buf.Len()), Queued: queued,
	}
	s.log.Info("demo: applied", "pair", p.ID, "ms", ms, "verified", res.Verified)
	writeJSON(w, http.StatusOK, res)
}

func (s *server) fail(w http.ResponseWriter, what string, err error) {
	s.log.Error("demo: "+what, "err", err)
	http.Error(w, what+": "+err.Error(), http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(b)
}

func clientIP(r *http.Request) string {
	// Fly puts the client address in Fly-Client-IP; behind nothing, the
	// remote address is it.
	if v := r.Header.Get("Fly-Client-IP"); v != "" {
		return v
	}
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// limiter is a token bucket per client, for the two comparison downloads
// only (docs/DEMO.md 3). It is deliberately crude: a map that is emptied
// when it grows, because the alternative to a demo being rate-limited badly
// is a demo with an egress bill.
type limiter struct {
	mu     sync.Mutex
	seen   map[string]*bucket
	burst  float64
	refill float64 // tokens per second
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter() *limiter {
	// Ten downloads, refilling one per six minutes.
	return &limiter{seen: map[string]*bucket{}, burst: 10, refill: 1.0 / 360}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.seen) > 10000 {
		l.seen = map[string]*bucket{}
	}
	now := time.Now()
	b := l.seen[key]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.seen[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.refill)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
