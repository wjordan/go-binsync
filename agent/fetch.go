package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/wjordan/go-binsync/delta"
	"github.com/wjordan/go-binsync/release"
	"github.com/wjordan/go-binsync/store"
)

// errVerify marks the failures that mean the bytes were not what the pointer
// promised. They are README exit code 3 and not something a retry fixes.
var errVerify = errors.New("verification failed")

// blobWorkers is how many ranged requests a blob fetch keeps in flight. One
// TCP stream carries about 1.2 Mbit/s at 1 % loss whatever the link rate, so
// the parallelism is most of what makes a full download bearable
// (docs/DESIGN.md 5).
const blobWorkers = 8

// fetchTries is how often one request is repeated before the cycle gives up
// and the next poll starts over.
const fetchTries = 5

// build produces the head release's bytes: the patch chain if the plan found
// one, the blob otherwise. A patch this build cannot read is not a failure —
// it falls back to the blob (README guarantee 3).
func (a *agent) build(ctx context.Context, p *release.Pointer, plan release.Plan) ([]byte, Outcome, error) {
	if plan.Kind == release.PlanChain {
		data, err := a.applyChain(ctx, plan)
		switch {
		case err == nil:
			return data, OutcomeUpdated, nil
		case !unsupportedTransform(err):
			return nil, outcomeFor(err), err
		case p.Head.Blob == nil:
			return nil, OutcomeNoPath, fmt.Errorf("agent: %w, and the pointer has no blob", err)
		}
		a.log.Warn("apply", "err", err, "fallback", "blob")
	}

	data, err := a.fetchBlob(ctx, p.Head)
	if err != nil {
		return nil, outcomeFor(err), err
	}
	return data, OutcomeUpdated, nil
}

func outcomeFor(err error) Outcome {
	if errors.Is(err, errVerify) {
		return OutcomeVerifyFailed
	}
	return OutcomeError
}

func unsupportedTransform(err error) bool {
	var u *delta.ErrUnsupportedTransform
	return errors.As(err, &u)
}

// applyChain walks the plan's edges oldest first, applying each patch to the
// bytes the previous one produced. delta.Apply checks the patch against the
// file it is given and its own output against the hash the patch promises,
// so a chain that reaches the end has produced the head release.
func (a *agent) applyChain(ctx context.Context, plan release.Plan) ([]byte, error) {
	cur, err := os.ReadFile(a.path)
	if err != nil {
		return nil, fmt.Errorf("agent: reading %s: %w", a.path, err)
	}
	for _, e := range plan.Edges {
		patch, err := a.object(ctx, e.Key, e.Size)
		if err != nil {
			return nil, err
		}
		if got := release.HashBytes(patch); got != e.B3 {
			return nil, fmt.Errorf("agent: patch %s hashes to %s, the pointer says %s: %w", e.Key, got, e.B3, errVerify)
		}
		out := bytes.NewBuffer(make([]byte, 0, len(cur)))
		if err := delta.Apply(cur, patch, out); err != nil {
			if unsupportedTransform(err) {
				return nil, err
			}
			return nil, fmt.Errorf("agent: applying %s: %w: %w", e.Key, errVerify, err)
		}
		a.log.Info("apply", "patch", e.Key, "bytes", len(patch), "to", e.To.String())
		cur = out.Bytes()
	}
	return cur, nil
}

// fetchBlob downloads the head's blob with blobWorkers ranged requests over
// the frame table, verifying each frame as it arrives and writing it at its
// offset. Frames already in hand are not fetched again, so the retry below
// resumes rather than restarts.
//
// A blob the pointer names but the store does not hold yet is not an error:
// the publisher uploads the patch and the pointer first, so that targets on
// the chain see a release without waiting for tens of MB (docs/DESIGN.md
// 4.4). Wait for it.
func (a *agent) fetchBlob(ctx context.Context, head release.Release) ([]byte, error) {
	b := head.Blob
	if err := b.Check(); err != nil {
		return nil, err
	}
	if b.PlainSize() != head.Size {
		return nil, fmt.Errorf("agent: %s describes %d bytes, the release is %d", b.Key, b.PlainSize(), head.Size)
	}
	out := make([]byte, head.Size)
	have := make([]bool, len(b.Frames))
	deadline := time.Now().Add(a.blobWait)
	start := time.Now()
	for wait := a.blobRetry; ; wait = min(2*wait, time.Minute) {
		err := a.fetchFrames(ctx, b, out, have)
		if err == nil {
			a.log.Info("fetch", "key", b.Key, "bytes", b.Size, "frames", len(b.Frames),
				"dur", time.Since(start).Round(time.Millisecond))
			return out, nil
		}
		if !errors.Is(err, store.ErrNotFound) || !time.Now().Before(deadline) {
			return nil, err
		}
		a.log.Info("fetch", "key", b.Key, "waiting", "the publisher has not uploaded it yet", "retry_in", wait)
		if !sleep(ctx, wait) {
			return nil, errors.Join(err, ctx.Err())
		}
	}
}

// fetchFrames runs one pass over the frames still missing. The first failure
// cancels the rest: a store that is down or a blob that is not there yet is
// not worth another 127 requests.
func (a *agent) fetchFrames(ctx context.Context, b *release.Blob, out []byte, have []bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	todo := make(chan int, len(b.Frames))
	missing := 0
	for i := range b.Frames {
		if !have[i] {
			todo <- i
			missing++
		}
	}
	close(todo)

	errs := make([]error, len(b.Frames))
	var wg sync.WaitGroup
	for w := 0; w < min(blobWorkers, missing); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Every frame is dispatched once, so the writes to have and errs
			// below are to elements no other worker touches.
			for i := range todo {
				if ctx.Err() != nil {
					return
				}
				if err := a.frame(ctx, b, i, out); err != nil {
					errs[i] = err
					cancel()
					return
				}
				have[i] = true
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// frame fetches [ZOff, ZOff+ZLen) of the blob object and writes the frame it
// decodes to at its place in out. DecodeFrame is where the pointer's hash is
// checked, before the bytes reach the decompressor.
func (a *agent) frame(ctx context.Context, b *release.Blob, i int, out []byte) error {
	f := b.Frames[i]
	for try := 1; ; try++ {
		z, err := a.get(ctx, b.Key, store.GetOptions{Off: f.ZOff, Len: f.ZLen}, f.ZLen)
		if err == nil {
			plain, err := release.DecodeFrame(f, z)
			if err != nil {
				return fmt.Errorf("agent: %s frame %d: %w: %w", b.Key, i, errVerify, err)
			}
			copy(out[f.Off:], plain)
			return nil
		}
		if try == fetchTries || errors.Is(err, store.ErrNotFound) || !sleep(ctx, a.retry<<(try-1)) {
			return err
		}
	}
}

// object reads a whole immutable object, repeating a transport failure a few
// times. want is the size the pointer names, which bounds the read.
func (a *agent) object(ctx context.Context, key string, want int64) ([]byte, error) {
	if want < 0 || want > a.max {
		return nil, fmt.Errorf("agent: %s: the pointer says %d bytes, the limit is %d", key, want, a.max)
	}
	start := time.Now()
	for try := 1; ; try++ {
		b, err := a.get(ctx, key, store.GetOptions{}, want)
		if err == nil {
			a.log.Info("fetch", "key", key, "bytes", len(b), "dur", time.Since(start).Round(time.Millisecond))
			return b, nil
		}
		if try == fetchTries || errors.Is(err, store.ErrNotFound) || !sleep(ctx, a.retry<<(try-1)) {
			return nil, err
		}
	}
}

// get reads exactly want bytes. An object longer or shorter than the pointer
// describes is refused rather than truncated: the hashes would catch it, but
// not before it had been allocated.
func (a *agent) get(ctx context.Context, key string, o store.GetOptions, want int64) ([]byte, error) {
	obj, err := a.st.Get(ctx, key, o)
	if err != nil {
		return nil, fmt.Errorf("agent: get %s: %w", key, err)
	}
	defer obj.Body.Close()
	b := make([]byte, want)
	if _, err := io.ReadFull(obj.Body, b); err != nil {
		return nil, fmt.Errorf("agent: get %s: %w", key, err)
	}
	var extra [1]byte
	if n, _ := obj.Body.Read(extra[:]); n > 0 {
		return nil, fmt.Errorf("agent: get %s: longer than the %d bytes the pointer names", key, want)
	}
	return b, nil
}

// readObject reads an object whose length the caller does not know yet — the
// pointer — refusing to grow past max.
func readObject(obj *store.Object, max int64) ([]byte, string, error) {
	defer obj.Body.Close()
	b, err := io.ReadAll(io.LimitReader(obj.Body, max+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(b)) > max {
		return nil, "", fmt.Errorf("longer than the %d byte limit", max)
	}
	return b, obj.ETag, nil
}
