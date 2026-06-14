// Command httpsproxy is a simple HTTPS (CONNECT) proxy for local use, with an
// allowlist specified on the command-line.
package httpsproxy

import (
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	listenAddr   string
	metricsAddr  string
	allowConnect string
	dialTimeout  time.Duration
	ioTimeout    time.Duration
)

type connectAddr struct {
	host string
	port int
}

var (
	connectAddrs = map[connectAddr]struct{}{}
	dialer       *net.Dialer
)

func parseAddr(s string) (connectAddr, error) {
	host, portstr, err := net.SplitHostPort(s)
	if err != nil {
		return connectAddr{}, fmt.Errorf("splitting address %q: %s", s, err)
	}
	port, err := strconv.ParseInt(portstr, 10, 32)
	if err != nil {
		return connectAddr{}, fmt.Errorf("bad port %q: %s", portstr, err)
	}
	return connectAddr{host, int(port)}, nil
}

func (a connectAddr) String() string {
	return net.JoinHostPort(a.host, fmt.Sprintf("%d", a.port))
}

var (
	metricsDisallowed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "httpsproxy_connect_disallowed_total",
		Help: "Total number of connect requests to disallowed addresses.",
	})
	metricsConnect = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "httpsproxy_connect_duration_seconds",
		Help: "Duration of connection.",
	}, []string{"address", "result"})
)

func HTTPSProxy(args []string) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewBuildInfoCollector(),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		metricsDisallowed,
		metricsConnect,
	)

	loglevel := slog.LevelInfo

	fl := flag.NewFlagSet("httpsproxy", flag.ExitOnError)

	log.SetFlags(0)
	fl.Usage = func() {
		log.Println("usage: gobuild httpproxy [flags]")
		fl.PrintDefaults()
		os.Exit(2)
	}
	fl.StringVar(&listenAddr, "listen", "127.0.0.1:8888", "address to serve on")
	fl.StringVar(&metricsAddr, "listen-metrics", "127.0.0.1:9888", "if non-empty, address to serve metrics on")
	fl.StringVar(&allowConnect, "allowconnect", "", "comma-separated list of addresses (host:port) to allow https connect to; required")
	fl.TextVar(&loglevel, "loglevel", loglevel, "log level, info prints connection requests")
	fl.DurationVar(&dialTimeout, "dialtimeout", 30*time.Second, "timeout for outgoing connections")
	fl.DurationVar(&ioTimeout, "iotimeout", 90*time.Second, "timeout for i/o on connections")
	fl.Parse(args)
	args = fl.Args()
	if len(args) != 0 {
		fl.Usage()
		os.Exit(2)
	}

	if allowConnect == "" {
		log.Fatalf("-allowconnect must be present and non-empty")
	}
	for s := range strings.SplitSeq(allowConnect, ",") {
		addr, err := parseAddr(s)
		if err != nil {
			log.Fatalf("bad address %q: %s", s, err)
		}
		connectAddrs[addr] = struct{}{}
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

	dialer = &net.Dialer{Timeout: dialTimeout}

	if metricsAddr != "" {
		go func() {
			mux := http.NewServeMux()
			mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
			slog.Info("listening for metrics", "addr", metricsAddr)
			err := http.ListenAndServe(metricsAddr, mux)
			slog.Error("listen and service metrics", "err", err)
		}()
	}

	slog.Info("listening", "addr", listenAddr)
	err := http.ListenAndServe(listenAddr, connectServer{})
	slog.Error("listen and serve", "err", err)
}

type connectServer struct{}

func (c connectServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	slog.Info("request", "method", r.Method, "url", r.URL)
	if r.Method != "CONNECT" {
		http.Error(w, "only connect allowed", http.StatusForbidden)
		return
	}

	addr, err := parseAddr(r.URL.Host)
	if err != nil {
		slog.Warn("bad address", "err", err, "url", r.URL)
		http.Error(w, fmt.Sprintf("400 - bad request - %s", err), http.StatusBadRequest)
		return
	}
	if _, ok := connectAddrs[addr]; !ok {
		metricsDisallowed.Inc()
		slog.Warn("address not allowed", "addr", addr)
		http.Error(w, "403 - forbidden - address not allowed", http.StatusForbidden)
		return
	}

	start := time.Now()
	result := "ok"
	defer func() {
		metricsConnect.WithLabelValues(addr.String(), result).Observe(float64(time.Since(start)) / float64(time.Second))
	}()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		result = "hijackerr"
		slog.Error("not a hijacker")
		http.Error(w, "400 - bad request - cannot hijack connection", http.StatusBadRequest)
		return
	}

	rconn, err := dialer.DialContext(r.Context(), "tcp", addr.String())
	if err != nil {
		result = "dialerr"
		slog.Info("connect to remote", "err", err, "addr", addr)
		http.Error(w, fmt.Sprintf("503 - service not available - connect to remote failed: %s", err), http.StatusServiceUnavailable)
		return
	}
	defer rconn.Close()

	w.WriteHeader(http.StatusOK)

	lconn, lbuf, err := hijacker.Hijack()
	if err != nil {
		result = "hijackerr2"
		slog.Error("hijack", "err", err)
		return
	}
	defer lconn.Close()

	var wg sync.WaitGroup

	// Copy from remote to local request.
	wg.Go(func() {

		buf := make([]byte, 32*1024)
		for {
			if rerr := rconn.SetReadDeadline(time.Now().Add(ioTimeout)); rerr != nil {
				slog.Warn("setting read deadline on connection to remote, continuing", "err", rerr)
			}
			n, rerr := rconn.Read(buf)
			if n > 0 {
				if lerr := lconn.SetWriteDeadline(time.Now().Add(ioTimeout)); lerr != nil {
					slog.Warn("setting write deadline on local connection, continuing", "err", lerr)
				}
				if _, lerr := lconn.Write(buf[:n]); lerr != nil {
					return
				}
			}
			if rerr == io.EOF {
				break
			} else if rerr != nil {
				return
			}
		}
	})

	// Copy from local request to remote.
	func() {
		nbuf := lbuf.Reader.Buffered()
		if nbuf > 0 {
			buf, err := lbuf.Peek(nbuf)
			if err != nil {
				slog.Warn("peek of local buffer, aborting", "err", err)
				return
			}
			if rerr := rconn.SetWriteDeadline(time.Now().Add(ioTimeout)); rerr != nil {
				slog.Warn("setting write deadline on connection to remote, continuing", "err", rerr)
			}
			if _, rerr := rconn.Write(buf); rerr != nil {
				return
			}
		}
		buf := make([]byte, 32*1024)
		for {
			if lerr := lconn.SetReadDeadline(time.Now().Add(ioTimeout)); lerr != nil {
				slog.Warn("setting read deadline on local connection, continuing", "err", lerr)
			}
			n, lerr := lconn.Read(buf)
			if n > 0 {
				if rerr := rconn.SetWriteDeadline(time.Now().Add(ioTimeout)); rerr != nil {
					slog.Warn("setting write deadline on connection to remote, continuing", "err", rerr)
				}
				if _, rerr := rconn.Write(buf[:n]); rerr != nil {
					return
				}
			}
			if lerr == io.EOF {
				break
			} else if lerr != nil {
				return
			}
		}
	}()

	wg.Wait()
}
