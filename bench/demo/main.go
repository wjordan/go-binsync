// Command demo serves the go-binsync patch explorer (docs/DEMO.md).
//
// It is a normal go-binsync target with a browser attached: the assets it serves
// are real stores built by `go-binsync publish` (bench/demo/build-assets.sh), the
// objects the page fetches are the objects an agent fetches, and the apply is
// delta.Apply against the same old binary. Nothing about the demo is a
// re-implementation of the product for demonstration purposes; if the page
// says the new binary verifies, a target would have installed it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	addr := flag.String("addr", envOr("ADDR", ":8080"), "listen address")
	dir := flag.String("assets", envOr("ASSETS", "/assets"), "asset directory (bench/demo/build-assets.sh writes it)")
	flag.Parse()

	pairs, err := loadPairs(*dir)
	if err != nil {
		log.Error("demo: loading assets", "err", err)
		os.Exit(1)
	}
	s := &server{
		pairs:   pairs,
		dir:     *dir,
		machine: envOr("FLY_MACHINE_ID", "local"),
		region:  envOr("FLY_REGION", "dev"),
		limit:   newLimiter(),
		apply:   make(chan struct{}, 1),
		log:     log,
	}
	for _, p := range pairs {
		log.Info("demo: pair", "id", p.ID, "old", p.OldSize, "patch", p.PatchSize,
			"hdiff", p.HdiffSize, "blob", p.BlobSize)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sd, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sd)
	}()
	log.Info("demo: listening", "addr", *addr, "region", s.region, "machine", s.machine,
		"pairs", len(pairs), "gomaxprocs", runtime.GOMAXPROCS(0))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("demo: serving", "err", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// pair is one release pair: a store to fetch from, an old binary to apply
// against, and the two comparison objects.
type pair struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Blurb    string `json:"blurb"`
	OldName  string `json:"old_name"`
	NewName  string `json:"new_name"`
	OldSize  int64  `json:"old_size"`
	NewSize  int64  `json:"new_size"`
	PatchKey string `json:"patch_key"`

	PatchSize int64  `json:"patch_size"`
	HdiffSize int64  `json:"hdiff_size"`
	BlobKey   string `json:"blob_key"`
	BlobSize  int64  `json:"blob_size"`
	FromHash  string `json:"from_hash"`
	ToHash    string `json:"to_hash"`
}

// loadPairs reads every meta.json under dir, in the order build-assets.sh
// writes them, and checks that the objects they name are really there -- a
// missing asset must stop the process, not surface as a 404 in a demo.
func loadPairs(dir string) ([]*pair, error) {
	order := []string{"one-line", "multi-package", "prometheus"}
	var out []*pair
	for _, id := range order {
		b, err := os.ReadFile(filepath.Join(dir, id, "meta.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		p := &pair{}
		if err := json.Unmarshal(b, p); err != nil {
			return nil, fmt.Errorf("%s/meta.json: %w", id, err)
		}
		for _, f := range []string{
			filepath.Join(dir, id, "old.bin"),
			filepath.Join(dir, id, "compare", "patch.hdiff"),
			filepath.Join(dir, id, "store", "latest.json"),
			filepath.Join(dir, id, "store", p.PatchKey),
			filepath.Join(dir, id, "store", p.BlobKey),
		} {
			if _, err := os.Stat(f); err != nil {
				return nil, err
			}
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no pairs under %s (run bench/demo/build-assets.sh)", dir)
	}
	return out, nil
}
