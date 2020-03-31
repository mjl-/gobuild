package main

import (
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mjl-/httpinfo"
	"github.com/mjl-/sconf"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/crypto/acme/autocert"
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
		HTTPS        *struct {
			ACME struct {
				Domains []string `sconf-doc:"List of domains to serve HTTPS for and request certificates for with ACME."`
				URL     string   `sconf:"optional" sconf-doc:"URL to ACME directory, default is the URL to Let's Encrypt."`
				Email   string   `sconf:"Contact email address to use when requesting certificates through ACME. CAs will contact this address in case of problems or expiry of certificates."`
				CertDir string   `sconf:"Directory to stored certificates in."`
			} `sconf-doc:"ACME configuration."`
		} `sconf:"optional" sconf-doc:"HTTPS configuration, if any."`
	}{
		"https://proxy.golang.org/",
		"data",
		"sdk",
		"home",
		0,
		nil,
		nil,
	}
)

var errRemote = errors.New("remote")
var errServer = errors.New("server error")

func serve(args []string) {
	serveFlags := flag.NewFlagSet("serve", flag.ExitOnError)

	listenAdmin := serveFlags.String("listen-admin", "localhost:8001", "address to serve admin-related http on")
	listenHTTP := serveFlags.String("listen-http", "localhost:8000", "address to serve plain http on")

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
		defer observePage("builds.txt", time.Now())
		http.ServeFile(w, r, path.Join(config.DataDir, "builds.txt"))
	})
	mux.HandleFunc("/m/", http.HandlerFunc(serveModules))
	mux.HandleFunc("/b/", http.HandlerFunc(serveBuild))
	mux.HandleFunc("/r/", http.HandlerFunc(serveResult))
	mux.HandleFunc("/img/gopher-dance-long.gif", func(w http.ResponseWriter, r *http.Request) {
		defer observePage("dance", time.Now())
		w.Header().Set("Content-Type", "image/gif")
		w.Write(fileGopherDanceLongGif) // nothing to do for errors
	})
	mux.HandleFunc("/", serveHome)
	msg := "listening on"
	if *listenHTTP != "" {
		msg += " http " + *listenHTTP
		go func() {
			log.Fatal(http.ListenAndServe(*listenHTTP, mux))
		}()
	}
	if config.HTTPS != nil {
		msg += " https :443"
		os.MkdirAll(config.HTTPS.ACME.CertDir, 0700) // errors will come up later
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(config.HTTPS.ACME.Domains...),
			Cache:      autocert.DirCache(config.HTTPS.ACME.CertDir),
			Email:      config.HTTPS.ACME.Email,
		}
		go func() {
			log.Fatal(http.Serve(m.Listener(), mux))
		}()
	}
	if *listenAdmin != "" {
		msg += " admin " + *listenAdmin
		go func() {
			log.Fatal(http.ListenAndServe(*listenAdmin, nil))
		}()
	}
	log.Print(msg)
	select {}
}

func failf(w http.ResponseWriter, format string, args ...interface{}) {
	err := fmt.Errorf(format, args...)
	msg := err.Error()
	if errors.Is(err, errServer) {
		log.Println(msg)
		http.Error(w, "500 - "+msg, http.StatusInternalServerError)
		return
	}
	http.Error(w, "400 - "+msg, http.StatusBadRequest)
}

func serveLog(w http.ResponseWriter, r *http.Request, p string) {
	f, err := os.Open(p)
	if err != nil {
		failf(w, "%w: open log.gz: %v", errServer, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	serveGzipFile(w, r, p, f)
}

func serveGzipFile(w http.ResponseWriter, r *http.Request, path string, src io.Reader) {
	if acceptsGzip(r) {
		w.Header().Set("Content-Encoding", "gzip")
		io.Copy(w, src) // nothing to do for errors
	} else {
		gzr, err := gzip.NewReader(src)
		if err != nil {
			failf(w, "%w: decompressing %q: %s", errServer, path, err)
			return
		}
		io.Copy(w, gzr) // nothing to do for errors
	}
}
