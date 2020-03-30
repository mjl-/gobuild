package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mjl-/httpinfo"
	"github.com/mjl-/sconf"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	workdir string
	homedir string

	recentBuilds struct {
		sync.Mutex
		paths []string
	}

	// We keep a map of available builds, so we can show in links in UI that navigating
	// won't trigger a build but will return quickly. The value indicates if the build was successful.
	availableBuilds = struct {
		sync.Mutex
		index map[string]bool // keys are urlPaths of build index requests, eg /b/mod@version/dir/goos-goarch-goversion/
	}{
		sync.Mutex{},
		map[string]bool{},
	}

	config = struct {
		GoProxy      string   `sconf-doc:"URL to proxy."`
		DataDir      string   `sconf-doc:"Directory where builds.txt and all builds files (binary, log, sha256) are stored."`
		SDKDir       string   `sconf-doc:"Directory where SDKs (go toolchains) are installed."`
		HomeDir      string   `sconf-doc:"Directory set as home directory during builds. Go caches will be created there."`
		MaxBuilds    int      `sconf-doc:"Maximum concurrent builds. Default (0) uses NumCPU+1."`
		VerifierURLs []string `sconf:"optional" sconf-doc:"URLs of other gobuild instances that are asked to perform the same build. Gobuild requires all of them to create the same binary for a successful build. Ideally, these instances differ in goos, goarch, user id and name, home and work directories."`
	}{
		"https://proxy.golang.org/",
		"data",
		"sdk",
		"home",
		0,
		nil,
	}
)

var (
	metricBuilds = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gobuild_builds_total",
			Help: "Number of builds.",
		},
		[]string{"goos", "goarch", "goversion"},
	)
	metricBuildErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gobuild_build_errors_total",
			Help: "Number of errors during builds.",
		},
		[]string{"goos", "goarch", "goversion"},
	)
	metricRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gobuild_requests_total",
			Help: "Number of requests per page.",
		},
		[]string{"page"},
	)
	metricHTTPModuleRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gobuild_http_module_request_duration_seconds",
			Help:    "Duration of requests on module endpoint in seconds.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 64, 128},
		},
		[]string{"code", "method"},
	)
	metricHTTPBuildRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gobuild_http_build_request_duration_seconds",
			Help:    "Duration of requests on build endpoint in seconds.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 64, 128},
		},
		[]string{"code", "method"},
	)
	metricHTTPResultRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gobuild_http_result_request_duration_seconds",
			Help:    "Duration of requests on result endpoint in seconds.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 64, 128},
		},
		[]string{"code", "method"},
	)
)

func main() {
	log.SetFlags(0)
	flag.Usage = func() {
		log.Println("usage: gobuild [flags] { config | testconfig | serve }")
		log.Println("       gobuild config")
		log.Println("       gobuild testconfig gobuild.conf")
		log.Println("       gobuild serve [gobuild.conf]")
		flag.PrintDefaults()
	}
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "config":
		err := sconf.Describe(os.Stdout, &config)
		if err != nil {
			log.Fatalf("describing config: %v", err)
		}
	case "testconfig":
		if len(args) != 1 {
			flag.Usage()
			os.Exit(2)
		}
		err := sconf.ParseFile(args[0], &config)
		if err != nil {
			log.Fatalf("parsing config file: %v", err)
		}
		log.Printf("config OK")
	case "serve":
		serve(args)
	default:
		flag.Usage()
		os.Exit(2)
	}
}

func serve(args []string) {
	serveFlags := flag.NewFlagSet("serve", flag.ExitOnError)
	listenAddress := serveFlags.String("listen", "localhost:8000", "address to serve on")
	listenAdmin := serveFlags.String("listenadmin", "localhost:8001", "address to serve admin-related http on")
	serveFlags.Usage = func() {
		log.Println("usage: gobuild serve [flags] [gobuild.conf]")
		serveFlags.PrintDefaults()
	}
	serveFlags.Parse(args)
	args = serveFlags.Args()
	if len(args) > 1 {
		serveFlags.Usage()
		os.Exit(2)
	}
	if len(args) > 0 {
		err := sconf.ParseFile(args[0], &config)
		if err != nil {
			log.Fatalf("parsing config file: %v", err)
		}
	}
	if !strings.HasSuffix(config.GoProxy, "/") {
		config.GoProxy += "/"
	}
	for i, url := range config.VerifierURLs {
		if strings.HasSuffix(url, "/") {
			config.VerifierURLs[i] = config.VerifierURLs[i][:len(config.VerifierURLs[i])-1]
		}
	}

	var err error
	workdir, err = os.Getwd()
	if err != nil {
		log.Fatalln("getwd:", err)
	}

	homedir = config.HomeDir
	if !path.IsAbs(homedir) {
		homedir = path.Join(workdir, config.HomeDir)
	}
	os.Mkdir(homedir, 0775) // failures will be caught later
	// We need a clean name: we will be match path prefixes against paths returned by
	// go tools, that will have evaluated names.
	homedir, err = filepath.EvalSymlinks(homedir)
	if err != nil {
		log.Fatalf("evaluating symlinks in homedir: %v", err)
	}
	os.Mkdir(config.SDKDir, 0775)  // may already exist, we'll get errors later
	os.Mkdir(config.DataDir, 0775) // may already exist, we'll get errors later

	initSDK()
	readRecentBuilds()

	go coordinateBuilds()

	http.Handle("/metrics", promhttp.Handler())
	http.Handle("/info", httpinfo.NewHandler(httpinfo.CodeVersion{}, nil))

	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "User-agent: *\nDisallow: /b/\n")
	})
	mux.HandleFunc("/builds.txt", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, path.Join(config.DataDir, "builds.txt"))
	})
	mux.HandleFunc("/m/", promhttp.InstrumentHandlerDuration(metricHTTPModuleRequestDuration, http.HandlerFunc(serveModules)))
	mux.HandleFunc("/b/", promhttp.InstrumentHandlerDuration(metricHTTPBuildRequestDuration, http.HandlerFunc(serveBuild)))
	mux.HandleFunc("/r/", promhttp.InstrumentHandlerDuration(metricHTTPResultRequestDuration, http.HandlerFunc(serveResult)))
	mux.HandleFunc("/img/gopher-dance-long.gif", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		w.Write(fileGopherDanceLongGif) // nothing to do for errors
	})
	mux.HandleFunc("/", serveHome)
	log.Printf("listening on %s and %s", *listenAddress, *listenAdmin)
	go func() {
		log.Fatalln(http.ListenAndServe(*listenAdmin, nil))
	}()
	log.Fatalln(http.ListenAndServe(*listenAddress, mux))
}

func serveHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != "GET" {
		http.Error(w, "405 - Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	metricRequestsTotal.WithLabelValues("home").Inc()

	recentBuilds.Lock()
	recents := recentBuilds.paths
	recentBuilds.Unlock()

	var args = struct {
		Recents []string
	}{
		recents,
	}
	err := homeTemplate.Execute(w, args)
	if err != nil {
		failf(w, "%w: executing template: %v", errServer, err)
	}
}
