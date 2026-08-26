package main

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"debug/elf"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"binsync/delta"
	"binsync/release"
	"binsync/store"
)

// casAttempts bounds the compare-and-swap loop on the pointer. Two publishers
// racing is rare and a third round means something is wrong, not busy.
const casAttempts = 3

func publish(ctx context.Context, log *slog.Logger, args []string) error {
	fs := newFlags("publish", "[--force] [--cache DIR] <binary> <store>")
	force := fs.Bool("force", false, "publish a binary that is not delta-friendly anyway")
	cacheDir := fs.String("cache", "", "release cache directory (default $XDG_CACHE_HOME/binsync)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return exitf(codeUsage, "publish needs a binary and a store URL")
	}
	binPath, storeURL := fs.Arg(0), fs.Arg(1)

	bin, err := os.ReadFile(binPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", binPath, err)
	}
	p := &publisher{log: log, bin: bin, hash: release.HashBytes(bin)}
	var warnings []string
	p.version, warnings = inspect(bin)
	for _, w := range warnings {
		log.Warn("binsync: " + w)
	}
	if len(warnings) > 0 && !*force {
		return fmt.Errorf("refusing to publish %s: it is not delta-friendly (see the warnings above); pass --force to publish anyway", binPath)
	}

	if p.cache, err = openCache(*cacheDir); err != nil {
		return err
	}
	if p.store, err = store.Open(storeURL); err != nil {
		return err
	}
	defer p.store.Close()

	log.Info("binsync: publishing", "release", p.hash, "version", p.version,
		"size", hbytes(int64(len(bin))), "store", p.store.URL())
	return p.run(ctx)
}

// publisher carries one publish: the binary, the blob built from it, and the
// store it all goes to.
type publisher struct {
	log     *slog.Logger
	store   store.Store
	cache   *releaseCache
	bin     []byte
	hash    release.Hash
	version string

	blobObj []byte
	blob    *release.Blob
}

// run is docs/DESIGN.md 4.4, in its order: finish an unfinished blob, stop if
// the head is already this release, then patch -> pointer -> blob, so targets
// on the chain see the release after hundreds of KB rather than tens of MB.
func (p *publisher) run(ctx context.Context) error {
	// The blob is built once, before the compare-and-swap loop: the patch
	// decision needs its size, the pointer needs its frame table, and
	// compressing it again per attempt would dominate a publish.
	start := time.Now()
	p.blobObj, p.blob = release.EncodeBlob(p.hash, p.bin)
	p.log.Info("binsync: compressed", "blob", hbytes(p.blob.Size),
		"of", hbytes(int64(len(p.bin))), "frames", len(p.blob.Frames), "took", hdur(time.Since(start)))

	for attempt := 1; ; attempt++ {
		prev, err := p.readPointer(ctx)
		if err != nil {
			return err
		}
		if err := p.finishBlob(ctx, prev); err != nil {
			return err
		}
		if prev != nil && prev.Head.Hash == p.hash {
			p.log.Info("binsync: already published", "release", p.hash)
			return nil
		}
		edge, err := p.putPatch(ctx, prev)
		if err != nil {
			return err
		}
		err = p.putPointer(ctx, prev, edge)
		if err == nil {
			break
		}
		if !errors.Is(err, store.ErrPreconditionFailed) || attempt == casAttempts {
			return err
		}
		p.log.Warn("binsync: another publisher replaced the pointer first; re-reading it", "attempt", attempt)
	}

	start = time.Now()
	if err := putObject(ctx, p.store, p.blob.Key, p.blobObj, immutable()); err != nil {
		return err
	}
	p.log.Info("binsync: uploaded the blob", "key", p.blob.Key, "size", hbytes(p.blob.Size), "took", hdur(time.Since(start)))
	if err := p.cache.put(p.hash, p.bin); err != nil {
		p.log.Warn("binsync: caching this release", "err", err)
	}
	return nil
}

// readPointer returns the store's current pointer, or nil for an empty store.
func (p *publisher) readPointer(ctx context.Context) (*release.Pointer, error) {
	b, etag, err := store.GetAll(ctx, p.store, release.PointerKey)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", release.PointerKey, err)
	}
	ptr, err := release.ParsePointer(b)
	if err != nil {
		return nil, err
	}
	ptr.ETag = etag
	return ptr, nil
}

// finishBlob uploads a blob a previous run left unfinished. The pointer goes
// up before the blob (docs/DESIGN.md D13), so a publisher that died in
// between leaves a head whose blob 404s, and the next publish is what heals
// it.
func (p *publisher) finishBlob(ctx context.Context, prev *release.Pointer) error {
	if prev == nil || prev.Head.Blob == nil {
		return nil
	}
	b := prev.Head.Blob
	if ok, err := exists(ctx, p.store, b.Key); err != nil || ok {
		return err
	}
	data := p.bin
	if prev.Head.Hash != p.hash {
		data = p.cache.get(prev.Head.Hash)
	}
	if data == nil {
		p.log.Warn("binsync: the head's blob is missing from the store and this machine has no copy of that release",
			"release", prev.Head.Hash, "key", b.Key)
		return nil
	}
	obj, nb := release.EncodeBlob(prev.Head.Hash, data)
	if nb.Size != b.Size || !sameFrames(nb.Frames, b.Frames) {
		// The pointer's frame hashes are what targets verify against, so
		// bytes that differ from them would be worse than the missing object.
		p.log.Warn("binsync: cannot reproduce the head's blob byte for byte; leaving it missing", "key", b.Key)
		return nil
	}
	p.log.Info("binsync: finishing the blob a previous publish left unfinished", "key", b.Key, "size", hbytes(int64(len(obj))))
	return putObject(ctx, p.store, b.Key, obj, immutable())
}

// putPatch encodes and uploads the patch from the previous head and returns
// the chain edge it adds. A nil edge publishes this release blob-only: there
// is no previous head, its bytes are out of reach, or the patch would not be
// smaller than the blob (README.md 4, guarantee 3).
func (p *publisher) putPatch(ctx context.Context, prev *release.Pointer) (*release.Edge, error) {
	if prev == nil {
		return nil, nil
	}
	old := p.oldBytes(ctx, prev)
	if old == nil {
		return nil, nil
	}
	from := prev.Head.Hash

	start := time.Now()
	var st delta.Stats
	patch, err := delta.Encode(old, p.bin, delta.Options{Stats: &st})
	if err != nil {
		return nil, fmt.Errorf("encoding the patch from %s: %w", from, err)
	}
	p.log.Info("binsync: encoded the patch", "size", hbytes(int64(len(patch))),
		"transform", st.Transform, "took", hdur(time.Since(start)))
	if int64(len(patch)) >= p.blob.Size {
		p.log.Info("binsync: the patch is not smaller than the blob; publishing blob-only",
			"patch", hbytes(int64(len(patch))), "blob", hbytes(p.blob.Size))
		return nil, nil
	}

	e := &release.Edge{
		From: from, To: p.hash,
		Key:  release.PatchKey(from, p.hash),
		Size: int64(len(patch)),
		B3:   release.HashBytes(patch),
	}
	return e, putObject(ctx, p.store, e.Key, patch, immutable())
}

// oldBytes is the previous head's binary, from the local cache or from its
// blob. Not having it costs the fleet a blob download, never correctness.
func (p *publisher) oldBytes(ctx context.Context, prev *release.Pointer) []byte {
	if b := p.cache.get(prev.Head.Hash); b != nil {
		return b
	}
	if prev.Head.Blob == nil {
		p.log.Warn("binsync: the previous release is neither cached here nor published as a blob; publishing blob-only",
			"release", prev.Head.Hash)
		return nil
	}
	p.log.Info("binsync: fetching the previous release to encode against", "release", prev.Head.Hash,
		"size", hbytes(prev.Head.Blob.Size))
	b, err := fetchBlob(ctx, p.store, prev.Head)
	if err != nil {
		p.log.Warn("binsync: could not fetch the previous release; publishing blob-only", "err", err)
		return nil
	}
	if err := p.cache.put(prev.Head.Hash, b); err != nil {
		p.log.Warn("binsync: caching the previous release", "err", err)
	}
	return b
}

// putPointer replaces the one mutable object with a compare-and-swap against
// the pointer this publish read, so two publishers cannot fork the chain.
func (p *publisher) putPointer(ctx context.Context, prev *release.Pointer, edge *release.Edge) error {
	np := &release.Pointer{
		Format: release.Format,
		Seq:    release.NewSeq(),
		Head:   release.Release{Hash: p.hash, Version: p.version, Size: int64(len(p.bin)), Blob: p.blob},
	}
	ifMatch := ""
	if prev != nil {
		ifMatch = prev.ETag
		// A target ignores a pointer whose seq did not grow, so a publisher
		// whose clock is behind the last one's must not use its own.
		if np.Seq <= prev.Seq {
			np.Seq = prev.Seq + 1
		}
		if edge != nil {
			np.Chain = append([]release.Edge{*edge}, prev.Chain...)
			np.Chain = np.Chain[:min(len(np.Chain), release.MaxChain)]
		}
	}
	b, err := np.Marshal()
	if err != nil {
		return err
	}
	if err := putObject(ctx, p.store, release.PointerKey, b, store.PutOptions{
		IfMatch:      &ifMatch,
		ContentType:  "application/json",
		CacheControl: "no-store",
	}); err != nil {
		return err
	}
	p.log.Info("binsync: published", "release", p.hash, "seq", np.Seq, "chain", len(np.Chain))
	return nil
}

// immutable is the metadata every content-addressed object carries: it can be
// cached until the heat death of the universe.
func immutable() store.PutOptions {
	return store.PutOptions{ContentType: "application/octet-stream", CacheControl: "public, max-age=31536000, immutable"}
}

func putObject(ctx context.Context, s store.Store, key string, b []byte, o store.PutOptions) error {
	o.Size = int64(len(b))
	if err := s.Put(ctx, key, bytes.NewReader(b), o); err != nil {
		return fmt.Errorf("putting %s: %w", key, err)
	}
	return nil
}

// inspect reads the version binsync reports for a release, and the reasons it
// will produce patches close to a full download (README.md 8).
func inspect(bin []byte) (version string, warnings []string) {
	if f, err := elf.NewFile(bytes.NewReader(bin)); err == nil {
		if f.Section(".symtab") != nil {
			warnings = append(warnings, `this binary carries a symbol table; rebuild with -ldflags="-s -w"`)
		}
		for _, s := range f.Sections {
			if strings.HasPrefix(s.Name, ".debug_") || strings.HasPrefix(s.Name, ".zdebug_") {
				warnings = append(warnings, `this binary carries DWARF debug info; rebuild with -ldflags="-s -w" -- unstripped, every patch is most of a full download`)
				break
			}
		}
		if f.Type == elf.ET_DYN {
			warnings = append(warnings, "this binary is position-independent; rebuild with -buildmode=exe for a smaller binary and smaller patches")
		}
	}
	bi, err := buildinfo.Read(bytes.NewReader(bin))
	if err != nil {
		return "", warnings
	}
	var rev string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				warnings = append(warnings, "this binary was built from a modified working tree; commit first, so the release hash is derivable from source")
			}
		}
	}
	version = bi.Main.Version
	if version == "" || version == "(devel)" {
		version = rev
		if len(version) > 12 {
			version = version[:12]
		}
	}
	return version, warnings
}
