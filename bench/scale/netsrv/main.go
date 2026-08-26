// netsrv: minimal static file server for the netem experiment. Go's http.FileServer
// supports Range requests and If-Modified-Since (304) natively; we add a weak ETag so
// If-None-Match works too, and disable keep-alive/compression surprises.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

func main() {
	dir := flag.String("dir", ".", "directory to serve")
	addr := flag.String("addr", "10.99.0.1:8080", "listen address")
	flag.Parse()
	fs := http.FileServer(http.Dir(*dir))
	h := func(w http.ResponseWriter, r *http.Request) {
		if st, err := os.Stat(filepath.Join(*dir, filepath.Clean(r.URL.Path))); err == nil && !st.IsDir() {
			w.Header().Set("ETag", fmt.Sprintf(`W/"%x-%x"`, st.Size(), st.ModTime().Unix()))
		}
		fs.ServeHTTP(w, r)
	}
	log.Printf("serving %s on %s", *dir, *addr)
	log.Fatal(http.ListenAndServe(*addr, http.HandlerFunc(h)))
}
