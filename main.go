// Gobuild serves reproducibly built binaries from sources via go module proxy.
//
// Serves URLs like:
//
// 	http://localhost:8000/
// 	http://localhost:8000/x/linux-amd64-go1.14.1/github.com/mjl-/sherpa/@v0.6.0/cmd/sherpaclient/{,log,sha256,dl}
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"sync"

	"github.com/mjl-/sconf"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	workdir string

	recentBuilds struct {
		sync.Mutex
		paths []string
	}

	// We keep a map of available builds, so we can show in links in UI that navigating
	// won't trigger a build but will return quickly. The value indicates if the build was successful.
	availableBuilds = struct {
		sync.Mutex
		index map[string]bool // keys are: goos-goarch-goversion/mod@version/dir
	}{
		sync.Mutex{},
		map[string]bool{},
	}

	config = struct {
		BaseURL string `sconf-doc:"Used to make full URLs in the web pages."`
		GoProxy string `sconf-doc:"URL to proxy, make sure it ends with a slash!."`
		DataDir string `sconf-doc:"Directory where builds.txt and all builds files (binaries, logs, sha256) are stored."`
		SDKDir  string `sconf-doc:"Directory where SDKs (go toolchains) are installed."`
		HomeDir string `sconf-doc:"Directory set as home directory during builds. Go caches will be created there."`
	}{
		"http://localhost:8000/",
		"https://proxy.golang.org/",
		"data",
		"sdk",
		"home",
	}
)

var (
	metricBuilds = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gobuild_builds_total",
			Help: "Number of builds.",
		},
		[]string{"goos", "goarch", "goversion"},
	)
	metricBuildErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gobuild_build_errors_total",
			Help: "Number of errors during builds.",
		},
		[]string{"goos", "goarch", "goversion"},
	)
	metricRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gobuild_requests_total",
			Help: "Number of requests per page.",
		},
		[]string{"page"},
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

	var err error
	workdir, err = os.Getwd()
	if err != nil {
		log.Fatalln("getwd:", err)
	}

	readRecentBuilds()

	http.Handle("/metrics", promhttp.Handler())

	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "User-agent: *\nDisallow: /x/\n")
	})
	mux.HandleFunc("/x/", serveBuilds)
	mux.HandleFunc("/", staticFile)
	log.Printf("listening on %s and %s", *listenAddress, *listenAdmin)
	go func() {
		log.Fatalln(http.ListenAndServe(*listenAdmin, nil))
	}()
	log.Fatalln(http.ListenAndServe(*listenAddress, mux))
}

func staticFile(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	metricRequestsTotal.WithLabelValues("home").Inc()

	recentBuilds.Lock()
	recents := recentBuilds.paths
	recentBuilds.Unlock()

	var args = struct {
		Recents []string
		BaseURL string
	}{
		recents,
		config.BaseURL,
	}
	err := homeTemplate.Execute(w, args)
	if err != nil {
		failf(w, "executing template: %v", err)
	}
}
