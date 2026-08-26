package store

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Only https is registered. Authenticity of a store is the endpoint's
// (docs/DESIGN.md 8), and http:// authenticates nothing, so a program that
// accepted it would silently publish a fleet's trust anchor to the wire.
func init() {
	Register("https", func(u *url.URL) (Store, error) { return newHTTPStore(u), nil })
}

// httpStore is a CDN or static server in front of a bucket: read-only, plain
// GETs, conditional and ranged.
type httpStore struct {
	base *url.URL
	c    *http.Client
}

// newHTTPStore takes any absolute URL; Open only ever hands it an https one.
func newHTTPStore(u *url.URL) *httpStore {
	base := *u
	base.RawQuery, base.Fragment = "", ""
	return &httpStore{base: &base, c: newHTTPClient()}
}

// newHTTPClient bounds every phase that can hang without bounding the whole
// request: a blob GET is tens of MB over a link that may carry 1 Mbit/s, and
// its deadline belongs to the caller's context.
func newHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 10 * time.Second
	t.ResponseHeaderTimeout = 30 * time.Second
	t.MaxIdleConnsPerHost = 8 // a blob is fetched as 8 parallel ranged GETs
	return &http.Client{
		Transport: t,
		// A redirect off https is a downgrade of the only thing that
		// authenticates the store.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != via[0].URL.Scheme {
				return fmt.Errorf("store: refusing redirect from %s to %s", via[0].URL.Scheme, req.URL.Scheme)
			}
			return nil
		},
	}
}

func (s *httpStore) URL() string { return s.base.String() }

func (s *httpStore) Close() error {
	s.c.CloseIdleConnections()
	return nil
}

func (s *httpStore) Get(ctx context.Context, key string, o GetOptions) (*Object, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	loc := s.base.JoinPath(key).String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loc, nil)
	if err != nil {
		return nil, fmt.Errorf("store: get %s: %w", loc, err)
	}
	if o.IfNoneMatch != "" {
		req.Header.Set("If-None-Match", o.IfNoneMatch)
	}
	ranged := o.Off > 0 || o.Len > 0
	switch {
	case o.Len > 0:
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", o.Off, o.Off+o.Len-1))
	case ranged:
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", o.Off))
	}
	resp, err := s.c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("store: get %s: %w", loc, err)
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
	case http.StatusNotModified:
		resp.Body.Close()
		return nil, fmt.Errorf("store: %s: %w", key, ErrNotModified)
	case http.StatusNotFound, http.StatusGone:
		resp.Body.Close()
		return nil, fmt.Errorf("store: %s: %w", key, ErrNotFound)
	default:
		resp.Body.Close()
		return nil, fmt.Errorf("store: get %s: %s", loc, resp.Status)
	}
	body, size := io.ReadCloser(resp.Body), resp.ContentLength
	if ranged && resp.StatusCode == http.StatusOK {
		// The server ignored the Range and sent the whole object: cut the
		// requested window out of the stream.
		if o.Off > 0 {
			if _, err := io.CopyN(io.Discard, body, o.Off); err != nil {
				body.Close()
				return nil, fmt.Errorf("store: get %s: seeking to %d: %w", loc, o.Off, err)
			}
			if size >= 0 {
				size = max(size-o.Off, 0)
			}
		}
		if o.Len > 0 {
			body = sectionCloser{io.LimitReader(body, o.Len), resp.Body}
			size = min(size, o.Len) // still -1 if the server gave no length
		}
	}
	return &Object{Body: body, Size: size, ETag: resp.Header.Get("ETag")}, nil
}

func (s *httpStore) Put(ctx context.Context, key string, r io.Reader, o PutOptions) error {
	return fmt.Errorf("store: put %s to %s: %w", key, s.base, ErrReadOnly)
}
