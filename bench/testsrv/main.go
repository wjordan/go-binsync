// Package main is a deliberately heavy Go web server used as a benchmark
// subject for binary delta experiments. Every import below is referenced
// from a handler so the linker keeps it.
package main

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	htmltpl "html/template"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/pprof"
	"net/rpc"
	"os"
	"regexp"
	"strconv"
	"strings"
	texttpl "text/template"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	bolt "go.etcd.io/bbolt"
	_ "modernc.org/sqlite"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

var (
	reqCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "bench_requests_total", Help: "requests",
	}, []string{"path"})
	wordRe = regexp.MustCompile(`\b([A-Za-z]+)\b`)
	textT  = texttpl.Must(texttpl.New("t").Parse("Hello {{.Name}}, you are {{.Age}} years old\n"))
	htmlT  = htmltpl.Must(htmltpl.New("h").Parse("<h1>{{.Name}}</h1><p>{{.Bio}}</p>"))
)

type person struct {
	XMLName xml.Name `xml:"person" json:"-"`
	Name    string   `xml:"name" json:"name"`
	Age     int      `xml:"age" json:"age"`
	Bio     string   `xml:"bio" json:"bio"`
}

// fakeDriver satisfies database/sql so the package is linked in.
type fakeDriver struct{}

func (fakeDriver) Open(name string) (driver.Conn, error) { return nil, errors.New("fake driver") }

// RPC service for net/rpc.
type Arith struct{}

func (Arith) Multiply(args *[2]int, reply *int) error { *reply = args[0] * args[1]; return nil }

func jsonHandler(w http.ResponseWriter, r *http.Request) {
	reqCounter.WithLabelValues("/json").Inc()
	var p person
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil && err != io.EOF {
		http.Error(w, err.Error(), 400)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(p)
}

func xmlHandler(w http.ResponseWriter, r *http.Request) {
	reqCounter.WithLabelValues("/xml").Inc()
	p := person{Name: "x", Age: 1}
	_ = xml.NewEncoder(w).Encode(p)
}

func gzipHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Encoding", "gzip")
	gz := gzip.NewWriter(w)
	defer gz.Close()
	fmt.Fprintf(gz, "compressed %s", r.URL.Path)
}

func zipHandler(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("a.txt")
	_, _ = f.Write([]byte("hello"))
	_ = zw.Close()
	_, _ = w.Write(buf.Bytes())
}

func pngHandler(w http.ResponseWriter, r *http.Request) {
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for i := 0; i < 16; i++ {
		img.Set(i, i, color.RGBA{255, 0, 0, 255})
	}
	_ = png.Encode(w, img)
}

func bigHandler(w http.ResponseWriter, r *http.Request) {
	n := new(big.Int)
	n.SetString(r.URL.Query().Get("n"), 10)
	f := new(big.Int).MulRange(1, 50)
	fmt.Fprintf(w, "%s %s\n", n.Exp(n, big.NewInt(3), nil), f)
}

func regexHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "%v\n", wordRe.FindAllString(r.URL.Query().Get("q"), -1))
}

func tmplHandler(w http.ResponseWriter, r *http.Request) {
	p := person{Name: r.URL.Query().Get("name"), Age: 42, Bio: "<b>bold</b>"}
	if r.URL.Query().Get("html") != "" {
		_ = htmlT.Execute(w, p)
		return
	}
	_ = textT.Execute(w, p)
}

func parseHandler(w http.ResponseWriter, r *http.Request) {
	src, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "in.go", src, parser.AllErrors)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	conf := types.Config{Error: func(error) {}}
	pkg, _ := conf.Check("p", fset, []*ast.File{f}, nil)
	fmt.Fprintf(w, "decls=%d pkg=%v\n", len(f.Decls), pkg != nil)
}

func sqlHandler(w http.ResponseWriter, r *http.Request) {
	drv := "sqlite"
	if r.URL.Query().Get("fake") != "" {
		drv = "fake"
	}
	db, err := sql.Open(drv, "file::memory:?cache=shared")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer db.Close()
	if _, err = db.ExecContext(r.Context(), "CREATE TABLE IF NOT EXISTS kv(k TEXT PRIMARY KEY, v TEXT)"); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	var n int
	err = db.QueryRowContext(r.Context(), "SELECT count(*) FROM kv").Scan(&n)
	fmt.Fprintf(w, "rows=%d err=%v\n", n, err)
}

func boltHandler(w http.ResponseWriter, r *http.Request) {
	db, err := bolt.Open(os.TempDir()+"/bench-bolt.db", 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer db.Close()
	err = db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("kv"))
		if err != nil {
			return err
		}
		return b.Put([]byte(r.URL.Path), []byte(strconv.FormatInt(time.Now().Unix(), 10)))
	})
	fmt.Fprintf(w, "bolt: %v\n", err)
}

func hashHandler(w http.ResponseWriter, r *http.Request) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	fmt.Fprintf(w, "%x\n", sha256.Sum256(b))
}

func s3Handler(w http.ResponseWriter, r *http.Request) {
	cfg, err := awscfg.LoadDefaultConfig(r.Context(), awscfg.WithRegion("us-east-1"))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) { o.BaseEndpoint = aws.String("http://127.0.0.1:1") })
	out, err := client.ListBuckets(r.Context(), &s3.ListBucketsInput{})
	if err != nil {
		fmt.Fprintf(w, "s3 error: %v\n", err)
		return
	}
	fmt.Fprintf(w, "buckets=%d\n", len(out.Buckets))
}

func tlsInfo() *tls.Config {
	pool, _ := x509.SystemCertPool()
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
}

func grpcServer() *grpc.Server {
	s := grpc.NewServer()
	hs := health.NewServer()
	healthpb.RegisterHealthServer(s, hs)
	reflection.Register(s)
	return s
}

func grpcClientProbe(ctx context.Context, addr string) error {
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer cc.Close()
	_, err = healthpb.NewHealthClient(cc).Check(ctx, &healthpb.HealthCheckRequest{})
	return err
}

func main() {
	sql.Register("fake", fakeDriver{})
	prometheus.MustRegister(reqCounter)
	_ = rpc.Register(Arith{})
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)
	r.Get("/", helloHandler)
	r.Post("/json", jsonHandler)
	r.Get("/xml", xmlHandler)
	r.Get("/gzip", gzipHandler)
	r.Get("/zip", zipHandler)
	r.Get("/png", pngHandler)
	r.Get("/big", bigHandler)
	r.Get("/re", regexHandler)
	r.Get("/tmpl", tmplHandler)
	r.Post("/parse", parseHandler)
	r.Get("/sql", sqlHandler)
	r.Get("/bolt", boltHandler)
	r.Get("/hash", hashHandler)
	r.Get("/s3", s3Handler)
	r.Handle("/metrics", promhttp.Handler())
	r.HandleFunc("/debug/pprof/*", pprof.Index)
	r.HandleFunc("/debug/pprof/profile", pprof.Profile)
	r.Handle(rpc.DefaultRPCPath, rpc.DefaultServer)

	addr := ":8080"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}
	gs := grpcServer()
	if ln, err := net.Listen("tcp", ":9090"); err == nil {
		go func() { _ = gs.Serve(ln) }()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := grpcClientProbe(ctx, "127.0.0.1:9090"); err != nil {
		logger.Info("grpc probe", "err", err.Error())
	}
	srv := &http.Server{Addr: addr, Handler: r, TLSConfig: tlsInfo(), ReadHeaderTimeout: 5 * time.Second}
	logger.Info("listening", "addr", addr, "upper", strings.ToUpper(addr))
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("serve", "err", err)
		os.Exit(1)
	}
}
