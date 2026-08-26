package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const httpPrefix = "/releases"

var httpBody = bytes.Repeat([]byte("0123456789"), 100) // 1000 B

// newTestStore serves one object under httpPrefix. When ignoreRange is set
// the server answers every request with the whole object, the way a static
// server that does not implement Range does.
func newTestStore(t *testing.T, ignoreRange bool) *httpStore {
	t.Helper()
	h := http.NewServeMux()
	h.HandleFunc(httpPrefix+"/blobs/x.zst", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"x-v1"`)
		if ignoreRange {
			w.Write(httpBody)
			return
		}
		http.ServeContent(w, r, "x.zst", time.Time{}, bytes.NewReader(httpBody))
	})
	h.HandleFunc(httpPrefix+"/boom", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL + httpPrefix)
	if err != nil {
		t.Fatal(err)
	}
	s := newHTTPStore(u)
	t.Cleanup(func() { s.Close() })
	return s
}

func TestHTTPSGet(t *testing.T) {
	t.Parallel()
	for _, ignoreRange := range []bool{false, true} {
		t.Run(map[bool]string{false: "ranged server", true: "server ignoring Range"}[ignoreRange], func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t, ignoreRange)
			ctx := context.Background()
			for _, tc := range []struct {
				name string
				o    GetOptions
				want []byte
			}{
				{"whole", GetOptions{}, httpBody},
				{"window", GetOptions{Off: 10, Len: 5}, httpBody[10:15]},
				{"to the end", GetOptions{Off: 990}, httpBody[990:]},
				{"past the end", GetOptions{Off: 995, Len: 100}, httpBody[995:]},
				{"first byte", GetOptions{Len: 1}, httpBody[:1]},
			} {
				t.Run(tc.name, func(t *testing.T) {
					obj, err := s.Get(ctx, "blobs/x.zst", tc.o)
					if err != nil {
						t.Fatalf("get: %v", err)
					}
					defer obj.Body.Close()
					got, err := io.ReadAll(obj.Body)
					if err != nil {
						t.Fatalf("read: %v", err)
					}
					if !bytes.Equal(got, tc.want) {
						t.Fatalf("got %d bytes, want %d", len(got), len(tc.want))
					}
					if obj.Size != int64(len(tc.want)) {
						t.Fatalf("Size = %d, read %d bytes", obj.Size, len(got))
					}
					if obj.ETag != `"x-v1"` {
						t.Fatalf("ETag = %q", obj.ETag)
					}
				})
			}
		})
	}
}

func TestHTTPSConditional(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, false)
	ctx := context.Background()
	if _, err := s.Get(ctx, "blobs/x.zst", GetOptions{IfNoneMatch: `"x-v1"`}); !errors.Is(err, ErrNotModified) {
		t.Fatalf("matching if-none-match: %v, want ErrNotModified", err)
	}
	b, etag, err := GetAll(ctx, s, "blobs/x.zst")
	if err != nil || !bytes.Equal(b, httpBody) || etag != `"x-v1"` {
		t.Fatalf("stale if-none-match: %d bytes, etag %q, %v", len(b), etag, err)
	}
}

func TestHTTPSErrors(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, false)
	ctx := context.Background()
	if _, err := s.Get(ctx, "blobs/missing.zst", GetOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing key: %v, want ErrNotFound", err)
	}
	err := s.Put(ctx, "latest.json", strings.NewReader("x"), PutOptions{})
	if !errors.Is(err, ErrReadOnly) {
		t.Fatalf("put: %v, want ErrReadOnly", err)
	}
	if _, err := s.Get(ctx, "boom", GetOptions{}); err == nil ||
		errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotModified) {
		t.Fatalf("500: %v, want a plain error", err)
	}
	if _, err := s.Get(ctx, "../etc/passwd", GetOptions{}); err == nil {
		t.Fatal("a key leaving the prefix was accepted")
	}
}

func TestHTTPSRedirects(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/here", http.StatusFound)
	})
	mux.HandleFunc("/here", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/downgraded", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.invalid/here", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	s := newHTTPStore(u)
	defer s.Close()

	if b, _, err := GetAll(context.Background(), s, "moved"); err != nil || string(b) != "ok" {
		t.Fatalf("same-scheme redirect: %q, %v", b, err)
	}
	// The test server is http, so a redirect to https stands in for the
	// https -> http downgrade the client must refuse.
	_, err := s.Get(context.Background(), "downgraded", GetOptions{})
	if err == nil || !strings.Contains(err.Error(), "refusing redirect") {
		t.Fatalf("cross-scheme redirect: %v, want a refusal", err)
	}
}

func TestHTTPSchemeRegistration(t *testing.T) {
	t.Parallel()
	s, err := Open("https://cdn.example/releases")
	if err != nil {
		t.Fatalf("open https: %v", err)
	}
	defer s.Close()
	if s.URL() != "https://cdn.example/releases" {
		t.Fatalf("URL() = %q", s.URL())
	}
	// http:// authenticates nothing, so it is not a store scheme.
	if _, err := Open("http://cdn.example/releases"); err == nil {
		t.Fatal("http:// was accepted")
	}
}
