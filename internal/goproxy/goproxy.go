package goproxy

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"errors"
	"flag"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	proxyCacheDir string
	goproxyURL    *url.URL
	cacheAdd      = make(chan int64, 100) // Size to add to cache.
)

func registerGoproxyMetrics() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(metricRequests)
	reg.MustRegister(metricCachableRequests)
	reg.MustRegister(metricCachedResponses)
	reg.MustRegister(metricErrors)
	reg.MustRegister(metricForwardErrors)
	return reg
}

var (
	metricRequests = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gobuild_goproxy_requests_total",
		Help: "Total number of requests to goproxy.",
	})
	metricCachableRequests = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gobuild_goproxy_cacheable_requests_total",
		Help: "Total number of requests to goproxy that could be in cache.",
	})
	metricCachedResponses = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gobuild_goproxy_cached_responses_total",
		Help: "Total number of requests to goproxy that were served from the cache.",
	})
	metricErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gobuild_goproxy_errors_total",
		Help: "Total number of internal errors during request and background cache handling to goproxy.",
	})
	metricForwardErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gobuild_goproxy_forward_errors_total",
		Help: "Total number of remote server errors while request handling to goproxy.",
	})
)

func Goproxy(args []string) {
	log.SetFlags(0)

	goproxyFlags := flag.NewFlagSet("goproxy", flag.ExitOnError)

	loglevel := slog.LevelInfo

	var (
		listenAdmin     string
		listenHTTP      string
		goproxyURLStr   string
		shutdownTimeout time.Duration
		cacheSize       int64
	)
	goproxyFlags.StringVar(&listenAdmin, "listen-admin", "localhost:8101", "address to serve admin-related http server on")
	goproxyFlags.StringVar(&listenHTTP, "listen-http", "localhost:8100", "address to serve plain http server on")
	goproxyFlags.StringVar(&goproxyURLStr, "goproxyurl", "https://proxy.golang.org", "goproxy url to forward requests to")
	goproxyFlags.DurationVar(&shutdownTimeout, "shutdown-timeout", 3*time.Second, "max time to do graceful shutdown of http servers")
	goproxyFlags.TextVar(&loglevel, "loglevel", loglevel, "log level, info prints connection requests")
	goproxyFlags.Int64Var(&cacheSize, "cachesize", 2*1024*1024*1024, "max total size for cached responses; old cached responses will be removed first")

	goproxyFlags.Usage = func() {
		log.Println("usage: gobuild goproxy [flags] cachedir")
		goproxyFlags.PrintDefaults()
	}
	goproxyFlags.Parse(args)
	args = goproxyFlags.Args()
	if len(args) != 1 {
		goproxyFlags.Usage()
		os.Exit(2)
	}

	var err error
	goproxyURL, err = url.Parse(goproxyURLStr)
	if err != nil {
		log.Fatalf("parsing goproxyurl %q: %v", goproxyURLStr, err)
	}

	slogOpts := slog.HandlerOptions{
		Level: loglevel,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == "time" {
				return slog.Attr{}
			}
			return a
		},
	}
	logHandler := slog.NewTextHandler(os.Stderr, &slogOpts)
	logger := slog.New(logHandler)
	slog.SetDefault(logger)

	gobuildVersion := "(no module)"
	if buildInfo, ok := debug.ReadBuildInfo(); ok {
		gobuildVersion = buildInfo.Main.Version
	}
	gobuildVersion += " " + runtime.Version()

	proxyCacheDir = args[0]
	if err := os.MkdirAll(proxyCacheDir, 0777); err != nil {
		log.Fatalf("ensuring directory %q exists: %v", proxyCacheDir, err)
	}

	// Cache size management is done by listing the files at startup and calculating
	// the total current size. Then waiting for "add" events (from requests), which
	// update the total cache size. Once we cross the max size, we trim it back by
	// listing all files and removing the oldest until we are at 90% of the max size.
	go func() {
		l, err := os.ReadDir(proxyCacheDir)
		if err != nil {
			log.Fatalf("listing proxy cache dir: %v", err)
		}
		var size int64
		for _, e := range l {
			fi, err := e.Info()
			if err != nil {
				log.Fatalf("stat %q: %v", e.Name(), err)
			}
			size += fi.Size()
		}
		slog.Info("listed cached files for current total size", "size", size)

		targetSize := 9 * cacheSize / 10

	Next:
		for {
			n := <-cacheAdd
			size += n
			if size <= cacheSize {
				continue
			}

			slog.Info("cache too large, cleaning up", "size", size, "maxsize", cacheSize, "targetsize", targetSize)

			// List all cached files, sort by mtime, oldest first.
			type file struct {
				name  string
				mtime time.Time
				size  int64
			}
			l, err := os.ReadDir(proxyCacheDir)
			if err != nil {
				metricErrors.Inc()
				slog.Error("reading dir to clean up cache, skipping", "err", err)
				continue Next
			}
			files := make([]file, 0, len(l))
			for _, e := range l {
				if strings.HasSuffix(e.Name(), ".tmp") {
					continue
				}
				fi, err := e.Info()
				if err != nil {
					metricErrors.Inc()
					slog.Error("stat cached file", "err", err, "name", e.Name())
					continue
				}
				files = append(files, file{e.Name(), fi.ModTime(), fi.Size()})
			}
			sort.Slice(files, func(i, j int) bool {
				return files[i].mtime.Before(files[j].mtime)
			})

			// Keep removing files (oldest first) as long as we are above target size.
			for len(files) > 0 && size > targetSize {
				f := files[0]
				files = files[1:]
				if err := os.Remove(filepath.Join(proxyCacheDir, f.name)); err != nil {
					metricErrors.Inc()
					slog.Error("removing cached file", "err", err, "name", f.name)
				} else {
					size -= f.size
				}
			}
			size = 0
			for _, f := range files {
				size += f.size
			}
			slog.Info("cache cleaned up", "size", size, "maxsize", cacheSize)
		}
	}()

	// Once we get a signal, we stop accepting new connections.
	acceptCtx, acceptCancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer acceptCancel()

	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	defer shutdownCancel()

	reg := registerGoproxyMetrics()
	http.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", serveGoProxy)

	httpErrorLog := slog.NewLogLogger(logHandler.WithAttrs([]slog.Attr{slog.String("httperr", "")}), slog.LevelInfo)
	slog.Info("gobuild goproxy",
		"httpaddr", listenHTTP,
		"adminaddr", listenAdmin,
		"version", gobuildVersion,
		"goversion", runtime.Version(),
		"goos", runtime.GOOS,
		"goarch", runtime.GOARCH)

	var servers []*http.Server // For calling Shutdown after the signal.
	var wgShutdown sync.WaitGroup

	if listenHTTP != "" {
		server := &http.Server{
			Addr: listenHTTP,
			BaseContext: func(net.Listener) context.Context {
				return shutdownCtx
			},
			Handler:  mux,
			ErrorLog: httpErrorLog,
		}
		servers = append(servers, server)
		wgShutdown.Add(1)
		go func() {
			defer wgShutdown.Done()
			err := server.ListenAndServe()
			if err == http.ErrServerClosed {
				slog.Info("listen and serve on http", "err", err)
			} else {
				slog.Error("listen and serve on http", "err", err)
				os.Exit(1)
			}
		}()
	}
	if listenAdmin != "" {
		server := &http.Server{
			Addr: listenAdmin,
			BaseContext: func(net.Listener) context.Context {
				return shutdownCtx
			},
			Handler:  http.DefaultServeMux,
			ErrorLog: httpErrorLog,
		}
		servers = append(servers, server)
		wgShutdown.Add(1)
		go func() {
			defer wgShutdown.Done()
			err := server.ListenAndServe()
			if err == http.ErrServerClosed {
				slog.Info("listen and serve on https", "err", err)
			} else {
				slog.Error("listen and serve on admin", "err", err)
				os.Exit(1)
			}
		}()
	}

	// When shutting down, make sure no modifications to transparency log are in progress.
	<-acceptCtx.Done()
	slog.Warn("shutdown after sigint or sigterm, waiting for connections/requests to finish", "timeout", shutdownTimeout)
	acceptCancel() // Ensure next ctrl-c stops immediately.

	// Shutdown webservers and other goroutines cleanly.
	stopCtx, stopCancel := context.WithTimeout(shutdownCtx, shutdownTimeout)
	defer stopCancel()

	for _, s := range servers {
		wgShutdown.Add(1)
		go func() {
			defer wgShutdown.Done()
			if err := s.Shutdown(stopCtx); err != nil {
				slog.Error("shutting down http server", "err", err)
			}
		}()
	}
	// Wait for http servers and select goroutines (e.g. for builds) to finish.
	wgShutdown.Wait()

	slog.Info("http servers and builds shut down, stopping other active operations and waiting 1s")
	shutdownCancel()
	time.Sleep(time.Second)

	slog.Info("exiting...")
}

func loggerCheck(log *slog.Logger, err error, msg string, args ...any) {
	if err != nil {
		metricErrors.Inc()
		args = append([]any{"err", err}, args...)
		log.Error(msg, args...)
	}
}

// statusWriter wraps an http.ResponseWriter, keeping track of the response status,
// for logging purposes.
type statusWriter struct {
	status int
	http.ResponseWriter
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(buf []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(buf)
}

// cacheWriter is an io.Writer that multiplexes writes to a file (for a cache to
// serve responses from in the future), and to the http client requesting the file.
// If a write to a client fails, the client is ignored and doesn't receive further
// writes (to ensure writes to the cache still make it in full).
type cacheWriter struct {
	cf     *os.File // Cache file.
	client io.Writer
}

func (c *cacheWriter) Write(buf []byte) (int, error) {
	n, err := c.cf.Write(buf)
	if n > 0 && c.client != nil {
		if _, err := c.client.Write(buf[:n]); err != nil {
			slog.Error("writing to client failed, no longer writing to client", "err", err)
			c.client = nil
		}
	}
	return n, err
}

// See https://go.dev/ref/mod#goproxy-protocol for the goproxy protocol.
// We just forward requests to the actual go proxy. With the exception of files
// ending in .zip, .mod, .info. Those don't change. The zip files are large. We
// don't cache "list" and "@latest" files at all, their contents can change.
func serveGoProxy(w http.ResponseWriter, r *http.Request) {
	log := slog.With("request", r.URL.Path)

	var fromcache bool
	sw := &statusWriter{ResponseWriter: w}
	w = sw
	defer func() {
		log.Info("goproxy request", "status", sw.status, "fromcache", fromcache)
	}()

	metricRequests.Inc()

	ext := filepath.Ext(r.URL.Path)
	var canCache bool
	var p string
	switch ext {
	case ".info", ".mod", ".zip":
		metricCachableRequests.Inc()
		canCache = true

		base := strings.TrimSuffix(r.URL.Path, ext)
		buf := sha256.Sum256([]byte(base))
		name := base64.RawURLEncoding.EncodeToString(buf[:]) + ext
		log = log.With("cachepath", name)
		p = filepath.Join(proxyCacheDir, name)
		f, err := os.Open(p)
		if err == nil {
			metricCachedResponses.Inc()
			fromcache = true
			defer f.Close()
			io.Copy(w, f)
			return
		}
		if !errors.Is(err, fs.ErrNotExist) {
			metricErrors.Inc()
			log.Error("attempt to open cached file, continuing", "err", err)
		}
	}

	u := *r.URL
	u.Scheme, u.Opaque, u.User, u.Host = goproxyURL.Scheme, goproxyURL.Opaque, goproxyURL.User, goproxyURL.Host
	nr, err := http.NewRequestWithContext(r.Context(), "GET", u.String(), nil)
	if err != nil {
		metricErrors.Inc()
		http.Error(w, "500 - bad request - cannot make http request: "+err.Error(), http.StatusInternalServerError)
		log.Error("making http request", "err", err)
		return
	}

	resp, err := http.DefaultClient.Do(nr)
	if err != nil {
		metricForwardErrors.Inc()
		http.Error(w, "502 - bad request - http transaction to goproxy: "+err.Error(), http.StatusBadGateway)
		log.Error("http transaction to goproxy", "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		metricForwardErrors.Inc()
	}
	if resp.StatusCode == http.StatusOK && canCache {
		// We'll write the entire file to disk, and copy it to the client as well, ignoring
		// errors when writing to the client.
		f, err := os.OpenFile(p+".tmp", os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o640)
		if err != nil {
			metricErrors.Inc()
			log.Error("creating cache file failed, continuing without cache", "err", err)
			io.Copy(w, resp.Body)
			return
		}
		defer func() {
			if f != nil {
				err := f.Close()
				loggerCheck(log, err, "closing tmp cache file")
				err = os.Remove(p + ".tmp")
				loggerCheck(log, err, "removing tmp cache file")
			}
		}()
		mw := &cacheWriter{f, w}
		size, err := io.Copy(mw, resp.Body)
		if err != nil {
			metricErrors.Inc()
			log.Error("writing cache file", "err", err)
			return
		}
		if err := f.Close(); err != nil {
			metricErrors.Inc()
			log.Error("closing cache file", "err", err)
			return
		}
		f = nil
		if err := os.Rename(p+".tmp", p); err != nil {
			metricErrors.Inc()
			log.Error("putting cache file in place", "err", err)
			err := os.Remove(p + ".tmp")
			loggerCheck(log, err, "removing temp cache file after failure")
		} else {
			log = log.With("tocache", true)
			cacheAdd <- size
		}
		return
	}

	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
