// Command srv is the service half of the handoff integration test: it takes
// its listener from go-binsync, answers every request with the version it was
// built with, and reports ready.
package main

import (
	"context"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"github.com/wjordan/go-binsync/selfupdate"
)

// version is stamped by the test's go build; addr is the pair both builds ask
// for, which is what makes the socket inheritable.
var version = "unset"

const addr = "127.0.0.1:0"

func main() {
	up := selfupdate.Start(selfupdate.Config{
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	ln, err := up.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("srv: listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, version)
	})}
	up.OnShutdown(func() { srv.Shutdown(context.Background()) })
	go srv.Serve(ln)

	// The test has no other handle on this process, and an orphan holding
	// the socket would outlive it.
	if err := os.WriteFile(os.Args[0]+".pid", []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		log.Fatalf("srv: %v", err)
	}
	up.Ready()
	<-up.Done()
}
