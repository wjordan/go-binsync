package selfupdate

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"binsync/release"
)

// The whole handoff against a real second build: this process serves, execs
// the built binary with its listening socket, and drains, while a client
// hammers the socket throughout. Nothing queued on the socket may be lost, so
// the client must see no error at all -- README guarantee 6.
func TestIntegrationHandoffLosesNoConnections(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "app")
	build(t, path)

	t.Cleanup(func() { kill(t, path) })

	u, _ := testUpdater(t, path, nil)
	ln, err := u.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "before")
	})}
	u.OnShutdown(func() { srv.Shutdown(context.Background()) })
	go srv.Serve(ln)
	u.Ready()

	url := "http://" + ln.Addr().String() + "/"
	client := &http.Client{
		// A fresh connection per request: every one of them has to be
		// accepted by whichever process owns the socket at that instant.
		Transport: &http.Transport{DisableKeepAlives: true},
		Timeout:   5 * time.Second,
	}
	var served, after atomic.Int64
	stop := make(chan struct{})
	var stopOnce sync.Once
	var wg sync.WaitGroup
	// Registered after the kill above, so the clients are joined before the
	// new process is taken away from under them.
	stopClients := func() { stopOnce.Do(func() { close(stop) }); wg.Wait() }
	t.Cleanup(stopClients)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				body, err := get(client, url)
				if err != nil {
					t.Errorf("request across the handoff: %v", err)
					return
				}
				served.Add(1)
				if body == "after" {
					after.Add(1)
				}
			}
		}()
	}

	h, err := release.HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The state an install leaves behind. The new process must read it as
	// "my parent is watching" and clear it from Ready; were it to read it as
	// a crashed release it would revert to the .old below, which is not a
	// server, and the handoff would fail.
	if err := os.WriteFile(u.inst.PendingPath(), []byte(h.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	writeScript(t, u.inst.OldPath(), "exit 9")
	if err := u.restart(h); err != nil {
		t.Fatalf("handoff: %v", err)
	}

	// The handoff returns once this process has drained; from here on every
	// request is answered by the new build on the same socket.
	deadline := time.Now().Add(5 * time.Second)
	for after.Load() < 100 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stopClients()

	if n := after.Load(); n < 100 {
		t.Errorf("the new build answered %d requests, want at least 100", n)
	}
	if n := served.Load(); n < 300 {
		t.Errorf("%d requests across the handoff, want at least 300", n)
	}
	select {
	case <-u.Done():
	default:
		t.Error("Done did not close after the handoff")
	}
	if exists(u.inst.PendingPath()) {
		t.Error("the new build did not clear the pending marker")
	}
	if got, err := release.HashFile(path); err != nil || got != h {
		t.Errorf("the binary at %s hashes to %v (%v), want %v -- something reverted it", path, got, err, h)
	}
	t.Logf("%d requests across the handoff, %d answered by the new build", served.Load(), after.Load())
}

func build(t *testing.T, out string) {
	t.Helper()
	start := time.Now()
	cmd := exec.Command("go", "build", "-ldflags=-X main.version=after", "-o", out, "./testdata/srv")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	t.Logf("built the test service in %v", time.Since(start).Round(time.Millisecond))
}

func get(client *http.Client, url string) (string, error) {
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func kill(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path + ".pid")
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatal(err)
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	p.Kill()
	p.Wait()
}
