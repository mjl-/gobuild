package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mjl-/gobuild/internal/sumdb"
	"github.com/mjl-/gobuild/ratelimit"

	"github.com/mjl-/sconf"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/mod/sumdb/note"
	"golang.org/x/mod/sumdb/tlog"
)

var (
	// Set to absolute paths: the config file can have relative paths.
	workdir  string
	homedir  string
	emptyDir string

	gobuildVersion  = "(no module)"
	gobuildPlatform = runtime.GOOS + "/" + runtime.GOARCH

	// We keep track of the 10 most recent successful builds to display on home page.
	recentBuilds struct {
		sync.Mutex
		links []string // as returned by request.urlPath
	}

	config = Config{
		"info",
		"https://proxy.golang.org/",
		"data",
		"sdk",
		"home",
		0,
		nil,
		nil,
		false,
		nil,
		nil,
		nil,
		"",
		"",
		"",
		nil,
		"",
		"",
		nil,
		"",
		0,
		Ratelimit{false},
		0,
		&slog.LevelVar{},
	}
	emptyConfig = config

	// Set to config.DataDir + "/result" after parsing config. Separate variable
	// because we use it quite a few times: for temp directories that we want nearby
	// (same partition) as final results.
	resultDir string

	// Opened at startup, used whenever we read/write to the hashes or records files.
	hashesFile, recordsFile *os.File

	// Either separate log file or stderr, append-only logging of added sums.
	sumLogFile io.Writer

	// Ratelimit for requests for builds with latest supported Go toolchains.
	limiterRecent = ratelimit.Limiter{
		IPClasses: [...][]int{
			{32, 24, 20, 16}, // IPv4
			{64, 48, 32, 24}, // IPv6
		},
		WindowLimits: []ratelimit.WindowLimit{
			{Window: time.Minute, Limits: []int64{10, 20, 80, 160}},
			{Window: 10 * time.Minute, Limits: []int64{20, 40, 160, 320}},
			{Window: 60 * time.Minute, Limits: []int64{30, 60, 240, 480}},
			{Window: 6 * 60 * time.Minute, Limits: []int64{40, 80, 400, 800}},
		},
	}
	// Ratelimit for builds with older Go toolchains. These are more likely done by
	// misbehaving crawler bots following all the links on a build page.
	limiterOld = ratelimit.Limiter{
		IPClasses: [...][]int{
			{32, 24, 20, 16}, // IPv4
			{64, 48, 32, 24}, // IPv6
		},
		WindowLimits: []ratelimit.WindowLimit{
			{Window: time.Minute, Limits: []int64{2, 3, 4, 5}},
			{Window: 10 * time.Minute, Limits: []int64{4, 6, 8, 10}},
			{Window: 60 * time.Minute, Limits: []int64{6, 9, 12, 15}},
			{Window: 6 * 60 * time.Minute, Limits: []int64{10, 15, 20, 25}},
		},
	}
)

type Config struct {
	LogLevel     string     `sconf:"optional" sconf-doc:"Log level: debug, info, warn, error. Default info."`
	GoProxy      string     `sconf-doc:"URL to Go module proxy. Used to resolve \"latest\" module versions."`
	DataDir      string     `sconf-doc:"Directory where the sumdb and builds files (binary, log) are stored. If it contains a robots.txt file, it is served for /robots.txt instead of the default."`
	SDKDir       string     `sconf-doc:"Directory where SDKs (go toolchains) are installed."`
	HomeDir      string     `sconf-doc:"Directory set as home directory during builds. Go will store its caches, downloaded and extracted modules here."`
	MaxBuilds    int        `sconf-doc:"Maximum concurrent builds. Default (0) uses NumCPU+1."`
	Environment  []string   `sconf:"optional" sconf-doc:"Additional environment variables in form KEY=VALUE to use for go command invocations. Useful to configure GOSUMDB and HTTPS_PROXY."`
	Run          []string   `sconf:"optional" sconf-doc:"Command and parameters to prefix invocations of go with. For example /usr/bin/nice."`
	BuildGobin   bool       `sconf-doc:"If enabled, sets environment variable GOBUILD_GOBIN during a build to a directory where the build command should write the binary. Configure a wrapper to the build command through the Run config option."`
	Verifiers    []Verifier `sconf:"optional" sconf-doc:"Other gobuild instances against which the result of a build is verified for being reproducible before adding them to the transparency log. Gobuild requires all of them to create the same binary (same hash) for a build to be successful. Ideally, these instances differ in hardware, goos, goarch, user id/name, home and work directories. Superseeds config field VerifierURLs."`
	VerifierURLs []string   `sconf:"optional" sconf-doc:"URLs of other gobuild instances that are asked to perform the same build. Gobuild requires all of them to create the same binary (same hash) for a build to be successful. Ideally, these instances differ in hardware, goos, goarch, user id/name, home and work directories. Superseded by Verifiers."`
	HTTPS        *struct {
		ACME struct {
			Domains []string `sconf-doc:"List of domains to serve HTTPS for and request certificates for with ACME."`
			Email   string   `sconf-doc:"Contact email address to use when requesting certificates through ACME. CAs will contact this address in case of problems or expiry of certificates."`
			CertDir string   `sconf-doc:"Directory to stored certificates in."`
		} `sconf-doc:"ACME configuration."`
	} `sconf:"optional" sconf-doc:"HTTPS configuration, if any."`
	SignerKeyFile                string          `sconf:"optional" sconf-doc:"File containing signer key as generated by subcommand genkey, for signing the transparent log."`
	VerifierKey                  string          `sconf:"optional" sconf-doc:"Verifier key as generated by subcommand genkey, for verifying a signed transparent log. This key is displayed on the home page."`
	LogDir                       string          `sconf-doc:"Directory to store log files. HTTP access logs are written, one file per day. Additions to the transparency logs, and HTTP protocol errors. Leave empty to disable logging."`
	ModulePrefixes               []string        `sconf:"optional" sconf-doc:"If non-empty, allow list of module prefixes for which binaries will be built. Requests for other module prefixes result in an error. Prefixes should typically end with a slash."`
	SDKVersionStop               string          `sconf:"optional" sconf-doc:"If set, the (hypothetical) version (and beyond) of the Go toolchain that is not allowed for builds. Gobuild automatically downloads new SDKs. However, new Go toolchain versions may change behaviour which may cause binaries to no longer become reproducible with the flags gobuild uses to build. By refusing new versions, you have time to separately verify binaries with newer Go toolchains are still reproducible. Example: a version of go1.20 allows go1.18, go1.19, go1.19.1, but not go1.20, go1.21 or go2.0. Versions like go1.20rc1 are interpreted as go1.20, without rc1."`
	InstanceNotesFile            string          `sconf:"optional" sconf-doc:"If set, a path to a plain text file with notes about this gobuild instance that is included on the main page."`
	BadClients                   []ClientPattern `sconf:"optional" sconf-doc:"Clients for which we won't start a new build. To prevent bad bots that ignore robots.txt from causing lots of builds."`
	BinaryCacheSizeMax           string          `sconf:"optional" sconf-doc:"Maximum size of the cache with built binaries. When adding a new binary would go over the size, binaries with the oldest access time are removed to stay under 90% of the maximum size. Value must end in MB or GB. The actual total size occupied by the binaries can temporarily exceed the configured maximum size. Can not be used with CleanupBinariesAccessTimeAge."`
	CleanupBinariesAccessTimeAge time.Duration   `sconf:"optional" sconf-doc:"Remove build result binaries with an access time longer this duration ago, if > 0. Binaries will be rebuilt, and verified to match the expected sum, when requested again. Can not be used with BinaryCacheSizeMax."`
	Ratelimit                    Ratelimit       `sconf:"optional" sconf-doc:"Requests for builds (compilation) through the web pages can be rate limited to prevent overloading the server. The lookups through the /tlog endpoints are not affected."`

	binaryCacheSizeMax int64 // Active if > 0.
	loglevel           *slog.LevelVar
}

// Verifier represents a different gobuild instance against builds are verified for
// reproducibility.
type Verifier struct {
	Key     string `sconf-doc:"Verifier key of gobuild instance."`
	BaseURL string `sconf:"optional" sconf-doc:"Base URL of gobuild instance. If empty, the name of the verifier key is used, with protocol https, and with /tlog appended."`

	name   string        `sconf:"-"` // From verifier key.
	client *sumdb.Client `sconf:"-"` // Initialized when configuration file is parsed.
}

type Ratelimit struct {
	Enabled bool `sconf-doc:"Whether rate limiting is enabled."`
}

// ClientPattern has fields for matching client requests.
type ClientPattern struct {
	HostnameSuffix string   `sconf:"optional" sconf-doc:"Hostname or suffix based on reverse DNS."`
	Networks       []string `sconf:"optional" sconf-doc:"IP networks, for matching request IP address."`
	UserAgent      string   `sconf:"optional" sconf-doc:"User-agent header substring match, case-insensitive."`

	ipnets []net.IPNet
}

var startTime = time.Now()

// Matches returns if a request matches the pattern.
func (cp ClientPattern) Match(r *http.Request) (hostname string, match bool) {
	if cp.UserAgent != "" {
		if !strings.Contains(strings.ToLower(r.UserAgent()), cp.UserAgent) {
			return "", false
		}
	}
	if len(cp.ipnets) == 0 && cp.HostnameSuffix == "" {
		return "", false
	}
	ipstr, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		slog.Debug("getting ip from remote address", "remoteaddr", r.RemoteAddr, "err", err)
		return "", false
	}
	if len(cp.ipnets) > 0 {
		found := false
		ip := net.ParseIP(ipstr)
		for _, ipnet := range cp.ipnets {
			if ipnet.Contains(ip) {
				found = true
				break
			}
		}
		if !found {
			return "", false
		}
	}
	if cp.HostnameSuffix != "" {
		// We'll try to lookup, but won't wait too long and fail open.
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		hosts, err := net.DefaultResolver.LookupAddr(ctx, ipstr)
		if err != nil {
			slog.Debug("reverse lookup for ip", "ip", ipstr, "err", err)
			return "", false
		}
		found := false
		for _, h := range hosts {
			h = strings.ToLower(h)
			if strings.HasSuffix(h, cp.HostnameSuffix) {
				found = true
				hostname = h
				break
			}
		}
		if !found {
			return "", false
		}
	}
	return hostname, true
}

var (
	//go:embed favicon.png
	fileFaviconPng []byte

	//go:embed favicon-building.png
	fileFaviconBuildingPng []byte

	//go:embed favicon-error.png
	fileFaviconErrorPng []byte

	// Dancing gopher, by Egon Elbre, CC0.  See https://github.com/egonelbre/gophers.
	//go:embed gopher-dance-long.gif
	fileGopherDanceLongGif []byte

	//go:embed template/base.html
	baseHTML string

	//go:embed template/build.html
	buildHTML string

	//go:embed template/module.html
	moduleHTML string

	//go:embed template/home.html
	homeHTML string

	//go:embed template/error.html
	errorHTML string
)

var (
	buildTemplate  = template.Must(template.New("build").Parse(buildHTML + baseHTML))
	moduleTemplate = template.Must(template.New("module").Parse(moduleHTML + baseHTML))
	homeTemplate   = template.Must(template.New("home").Parse(homeHTML + baseHTML))
	errorTemplate  = template.Must(template.New("error").Parse(errorHTML))
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
		if err := parseConfig(args[0], &config); err != nil {
			log.Fatalf("parsing config file: %v", err)
		}
	}
	if !strings.HasSuffix(config.GoProxy, "/") {
		config.GoProxy += "/"
	}
	for i, v := range config.Verifiers {
		// Parse verifier key ourselves so we can get the name, which we use for logging.
		if nv, err := note.NewVerifier(strings.TrimSpace(string(v.Key))); err != nil {
			log.Fatalf("parsing verifier key %s: %v", v.Key, err)
		} else {
			v.name = nv.Name()
		}

		// Store a sumdb.Client in the verifier, for when we need to do the verification.
		client, _, err := newClient(v.Key, v.BaseURL)
		if err != nil {
			log.Fatalf("parsing key %s of verifier: %v", v.Key, err)
		}
		config.Verifiers[i].client = client
	}
	if len(config.VerifierURLs) > 0 {
		slog.Warn("VerifierURLs has been superseded by Verifiers, for which the remote's transparency log is used for verification. Consider updating your configuration.")
	}
	for i, url := range config.VerifierURLs {
		config.VerifierURLs[i] = strings.TrimSuffix(url, "/")
	}
	resultDir = filepath.Join(config.DataDir, "result")
	if config.SDKVersionStop != "" {
		v, err := parseGoVersion(config.SDKVersionStop)
		if err != nil {
			log.Fatalf("parsing MaxSDKVersion %q from config: %s", config.SDKVersionStop, err)
		}
		sdkVersionStop = &v
	}

	if buildInfo, ok := debug.ReadBuildInfo(); ok {
		gobuildVersion = buildInfo.Main.Version
	}
	gobuildVersion += " " + runtime.Version()

	var err error
	workdir, err = os.Getwd()
	if err != nil {
		log.Fatalln("getwd:", err)
	}

	homedir = config.HomeDir
	if !filepath.IsAbs(homedir) {
		homedir = filepath.Join(workdir, config.HomeDir)
	}
	os.Mkdir(homedir, 0777) // failures will be caught later
	// We need a clean name: we will be matching path prefixes against paths returned by
	// go tools, that will have evaluated names.
	homedir, err = filepath.EvalSymlinks(homedir)
	if err != nil {
		log.Fatalf("evaluating symlinks in homedir: %v", err)
	}
	emptyDir = filepath.Join(homedir, "tmp")
	os.MkdirAll(emptyDir, 0555)
	os.MkdirAll(config.SDKDir, 0777)                        // may already exist, we'll get errors later
	os.MkdirAll(filepath.Join(config.DataDir, "sum"), 0777) // may already exist, we'll get errors later

	// Make directories for each leading char for urlsafe base64 data, for storing results.
	os.MkdirAll(resultDir, 0777) // may already exist, we'll get errors later
	mksumdir := func(c rune) {
		os.MkdirAll(filepath.Join(resultDir, string(c)), 0777)
	}
	for c := 'a'; c <= 'z'; c++ {
		mksumdir(c)
	}
	for c := 'A'; c <= 'Z'; c++ {
		mksumdir(c)
	}
	for c := '0'; c <= '9'; c++ {
		mksumdir(c)
	}
	mksumdir('-')
	mksumdir('_')

	// Open data/sum/hashes and data/sum/records files for the lifetime of the program.
	// Creating empty files is proper initialization.
	hashesPath := filepath.Join(config.DataDir, "sum", "hashes")
	hashesFile, err = os.OpenFile(hashesPath, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		log.Fatalf("creating hashes file: %v", err)
	}
	recordsPath := filepath.Join(config.DataDir, "sum", "records")
	recordsFile, err = os.OpenFile(recordsPath, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		log.Fatalf("creating records file: %v", err)
	}

	// Verify the most recent additions to the records & hashes files are consistent.
	if recordCount, err := verifySumState(); err != nil {
		log.Fatal(err)
	} else {
		metricTlogRecords.Set(float64(recordCount))
	}

	// Lower limits on http DefaultTransport. We typically only connect to a few
	// places, so we can keep fewer idle connections, and for shorter period.
	defaultTransport := http.DefaultTransport.(*http.Transport)
	defaultTransport.MaxIdleConns = 10
	defaultTransport.IdleConnTimeout = 30 * time.Second
	defaultTransport.DialContext = (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext

	initSDK()
	readRecentBuilds()

	go func() {
		// If coordinateBuilds panics, we will grind to a halt, but at least we'll get alerting about it.
		defer logPanic()

		coordinateBuilds()
	}()

	// When shutting down, make sure no modifications to transparency log are in progress.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigc
		addSumMutex.Lock()
		log.Fatal("shutdown after sigint or sigterm")
	}()

	if config.BinaryCacheSizeMax != "" && config.CleanupBinariesAccessTimeAge > 0 {
		log.Fatalf("cannot configure both BinaryCacheSizeMax and CleanupBinariesAccessTimeAge")
	}

	if config.BinaryCacheSizeMax != "" {
		// Parse the size.
		s := strings.ToLower(config.BinaryCacheSizeMax)
		s = strings.TrimSuffix(s, "b")
		var mult int64
		if strings.HasSuffix(s, "m") {
			mult = 1024 * 1024
		} else if strings.HasSuffix(s, "g") {
			mult = 1024 * 1024 * 1024
		} else {
			log.Fatalf("invalid value %q for BinaryCacheSizeMax: must end with mb or gb", config.BinaryCacheSizeMax)
		}
		v, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
		if err != nil {
			log.Fatalf("parsing %q for BinaryCacheSizeMax: %v", config.BinaryCacheSizeMax, err)
		}
		config.binaryCacheSizeMax = mult * v

		// Check we know the current cache size. If not, we walk the data result/ directory
		// and calculate it.
		buf, err := os.ReadFile(filepath.Join(config.DataDir, "result", "binary-cache-size.txt"))
		if err == nil {
			binaryCache.Lock()
			binaryCache.size, err = strconv.ParseInt(strings.TrimSpace(string(buf)), 10, 64)
			if err == nil {
				slog.Info("binary cache size read from file", "size", binaryCache.size)
			}
			binaryCache.Unlock()
		}
		if err != nil {
			slog.Info("cannot read or parse result/binary-cache-size.txt, will walk result and initialize file", "err", err)

			// Walk data's result/ dir to gather the current size. We won't remove anything. It
			// will write a new binary-cache-size.txt file and set binaryCache.size.
			err := binaryCacheCleanup(0)
			if err != nil {
				slog.Error("determining current size of binaries cache", "err", err)
			}
		}

		// We may need to cleanup now.
		if reclaim := binaryCacheSizeAdd(0); reclaim > 0 {
			go func() {
				defer logPanic()

				err := binaryCacheCleanup(reclaim)
				if err != nil {
					slog.Error("cleaning up binary cache at startup", "err", err, "reclaim", reclaim)
				}
			}()
		}
	}
	if config.CleanupBinariesAccessTimeAge > 0 {
		go func() {
			defer logPanic()
			time.Sleep(time.Minute)
			for {
				cleanupBinariesAtime(config.CleanupBinariesAccessTimeAge)
				time.Sleep(24 * time.Hour)
			}
		}()
	}

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/pending", func(w http.ResponseWriter, r *http.Request) {
		// Return list of pending builds, including IP addresses.
		rc := make(chan coordinatorState)
		coordinate.state <- rc
		state := <-rc

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		for _, elem := range state.queue {
			fmt.Fprintf(w, "%s %s\n", elem.buildSpec.String(), elem.initiator.String())
		}
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		f, err := os.Open(filepath.Join(config.DataDir, "robots.txt"))
		if err == nil {
			defer f.Close()
			http.ServeContent(w, r, "robots.txt", startTime, f)
		} else {
			// Use of "*" may not be understood by all bots. There is no explicit allowlist. So
			// we end up just disallowing everything.
			fmt.Fprint(w, "User-agent: *\nDisallow: /\n")
		}
	})
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(fileFaviconPng) // nothing to do for errors
	})
	mux.HandleFunc("/favicon-building.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(fileFaviconBuildingPng) // nothing to do for errors
	})
	mux.HandleFunc("/favicon-error.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(fileFaviconErrorPng) // nothing to do for errors
	})

	mux.HandleFunc("/emptyconfig", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		sconf.Describe(w, &emptyConfig) // nothing to do for errors
	})

	mux.HandleFunc("/buildfailures.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		http.ServeFile(w, r, filepath.Join(config.DataDir, "buildfailures.txt"))
	})

	if config.SignerKeyFile != "" {
		skey, err := os.ReadFile(config.SignerKeyFile)
		if err != nil {
			log.Fatalf("reading signer key: %v", err)
		}
		signer, err := note.NewSigner(string(skey))
		if err != nil {
			log.Fatalf("new signer: %v", err)
		}

		h := httpRemoteAddrHandler{http.StripPrefix("/tlog", sumdb.NewServer(serverOps{signer}))}
		for _, path := range sumdb.ServerPaths {
			mux.Handle("/tlog"+path, h)
		}
	}

	mux.HandleFunc("/img/gopher-dance-long.gif", func(w http.ResponseWriter, r *http.Request) {
		defer observePage("dance", time.Now())
		w.Header().Set("Content-Type", "image/gif")
		w.Write(fileGopherDanceLongGif) // nothing to do for errors
	})

	// These prefixes are old. We still serve on these paths for compatibility.
	mux.HandleFunc("/m/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path[2:], http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/b/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path[2:], http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/r/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path[2:], http.StatusTemporaryRedirect)
	})

	mux.HandleFunc("/", serveHome)

	slogOpts := slog.HandlerOptions{
		Level: config.loglevel,
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

	var handler http.Handler = mux
	var httpErrorLog *log.Logger
	if config.LogDir != "" {
		os.MkdirAll(config.LogDir, 0777)
		handler = newLogHandler(mux, config.LogDir)

		sumLogFile, err = os.OpenFile(filepath.Join(config.LogDir, "sum.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0666)
		if err != nil {
			log.Fatalf("open sum.log: %v", err)
		}

		if httperror, err := os.OpenFile(filepath.Join(config.LogDir, "httperror.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0666); err != nil {
			log.Fatalf("open httperror.log: %v", err)
		} else {
			httpErrorLog = log.New(httperror, "", log.LstdFlags)
		}

	} else {
		sumLogFile = os.Stderr
		httpErrorLog = slog.NewLogLogger(logHandler.WithAttrs([]slog.Attr{slog.String("httperr", "")}), slog.LevelInfo)
	}

	var httpsaddr string
	if config.HTTPS != nil {
		httpsaddr = ":443"
	}
	slog.Info("starting gobuild", "httpaddr", *listenHTTP, "httpsaddr", httpsaddr, "adminaddr", *listenAdmin, "version", gobuildVersion, "goversion", runtime.Version(), "goos", runtime.GOOS, "goarch", runtime.GOARCH)

	if *listenHTTP != "" {
		go func() {
			server := &http.Server{
				Addr:     *listenHTTP,
				Handler:  handler,
				ErrorLog: httpErrorLog,
			}
			err := server.ListenAndServe()
			slog.Error("listen and serve on http", "err", err)
			os.Exit(1)
		}()
	}
	if config.HTTPS != nil {
		os.MkdirAll(config.HTTPS.ACME.CertDir, 0700) // errors will come up later
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(config.HTTPS.ACME.Domains...),
			Cache:      autocert.DirCache(config.HTTPS.ACME.CertDir),
			Email:      config.HTTPS.ACME.Email,
		}
		go func() {
			server := &http.Server{
				Handler:  handler,
				ErrorLog: httpErrorLog,
			}
			err := server.Serve(m.Listener())
			slog.Error("listen and serve on https", "err", err)
			os.Exit(1)
		}()
	}
	if *listenAdmin != "" {
		go func() {
			err := http.ListenAndServe(*listenAdmin, nil)
			slog.Error("listen and serve on admin", "err", err)
			os.Exit(1)
		}()
	}
	select {}
}

func logPanic() {
	x := recover()
	if x == nil {
		return
	}

	metricPanics.Inc()
	slog.Error("unhandled panic", "panic", x)
	debug.PrintStack()
}

func logCheck(err error, msg string, args ...any) {
	if err != nil {
		args = append([]any{"err", err}, args...)
		slog.Error(msg, args...)
	}
}

func failf(w http.ResponseWriter, format string, args ...any) {
	err := fmt.Errorf(format, args...)
	errmsg := err.Error()
	if isClosed(err) || strings.HasSuffix(errmsg, "http2: stream closed") || strings.HasSuffix(errmsg, "client disconnected") {
		// The http2 error seems to happen when remote closes the connection. No point logging.
		return
	}
	var status int
	if errors.Is(err, errRemote) {
		status = http.StatusServiceUnavailable
	} else if errors.Is(err, errServer) {
		status = http.StatusInternalServerError
	} else {
		status = http.StatusBadRequest
	}
	statusfail(status, w, errmsg)
}

func statusfail(status int, w http.ResponseWriter, errmsg string) {
	msg := fmt.Sprintf("%d - %s - %s", status, http.StatusText(status), errmsg)
	if status/100 == 5 {
		metricHTTPRequestsServerErrors.Inc()
		slog.Error("http server error", "status", status, "err", errmsg)
		debug.PrintStack()
	} else {
		metricHTTPRequestsUserErrors.Inc()
		slog.Debug("http user error", "status", status, "err", errmsg)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	err := errorTemplate.Execute(w, map[string]string{"Message": msg})
	if err != nil {
		slog.Error("executing template for error", "err", err)
	}
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
	} else if gzr, err := gzip.NewReader(src); err != nil {
		failf(w, "%w: decompressing %q: %s", errServer, path, err)
	} else {
		io.Copy(w, gzr) // nothing to do for errors
	}
}

func acceptsGzip(r *http.Request) bool {
	s := r.Header.Get("Accept-Encoding")
	t := strings.Split(s, ",")
	for _, e := range t {
		e = strings.TrimSpace(e)
		tt := strings.Split(e, ";")
		if len(tt) > 1 && t[1] == "q=0" {
			continue
		}
		if tt[0] == "gzip" {
			return true
		}
	}
	return false
}

func verifySumState() (int64, error) {
	// Verify records & hashes files have consistent sizes.
	numRecords, err := treeSize()
	if err != nil {
		return -1, fmt.Errorf("finding number of records in tlog: %v", err)
	}
	if info, err := hashesFile.Stat(); err != nil {
		return -1, fmt.Errorf("stat on hashes file: %v", err)
	} else if hashCount := tlog.StoredHashCount(numRecords); hashCount*tlog.HashSize != info.Size() {
		return -1, fmt.Errorf("inconsistent size of hashes file of %d bytes for %d records, should be %d", info.Size(), numRecords, hashCount*tlog.HashSize)
	}

	// For the latest record on disk, verify the hashes on disk match the record.
	if numRecords == 0 {
		return 0, nil
	}

	lastRecordNum := numRecords - 1
	records, err := serverOps{}.ReadRecords(context.Background(), lastRecordNum, 1)
	if err != nil {
		return -1, fmt.Errorf("reading last record: %v", err)
	}
	hashes, err := tlog.StoredHashes(lastRecordNum, records[0], hashReader{})
	if err != nil {
		return -1, fmt.Errorf("calculating hashes for most recent record: %v", err)
	}
	buf := make([]byte, len(hashes)*tlog.HashSize)
	if _, err := hashesFile.ReadAt(buf, tlog.StoredHashIndex(0, lastRecordNum)*tlog.HashSize); err != nil {
		return -1, fmt.Errorf("reading hashes for verification: %v", err)
	}
	for i := range hashes {
		o := i * tlog.HashSize
		h := buf[o : o+tlog.HashSize]
		if !bytes.Equal(hashes[i][:], h) {
			return -1, fmt.Errorf("hash %d mismatch for last record %d, got %x, expect %x", i, lastRecordNum, h, hashes[i][:])
		}
	}

	// Also check if the recordnumber file is available, i.e. if a lookup will succeed.
	record, err := parseRecord(records[0])
	if err != nil {
		return -1, fmt.Errorf("parsing last record: %v", err)
	}
	if buf, err := os.ReadFile(filepath.Join(record.storeDir(), "recordnumber")); err != nil {
		return -1, fmt.Errorf("open recordnumber: %v", err)
	} else if num, err := strconv.ParseInt(string(buf), 10, 64); err != nil {
		return -1, fmt.Errorf("parse recordnumber from file: %v", err)
	} else if num != lastRecordNum {
		return -1, fmt.Errorf("inconsistent last recordnumber %d, expected %d", num, lastRecordNum)
	}

	// And check if the hash of the binary matches the sum.
	h := sha256.New()
	f, err := os.Open(filepath.Join(record.storeDir(), "binary.gz"))
	if err != nil && errors.Is(err, fs.ErrNotExist) {
		return numRecords, nil
	} else if err != nil {
		return -1, fmt.Errorf("open binary.gz for verification: %v", err)
	}
	defer f.Close()
	if gzr, err := gzip.NewReader(f); err != nil {
		return -1, fmt.Errorf("gzip reader for binary.gz: %v", err)
	} else if _, err := io.Copy(h, gzr); err != nil {
		return -1, fmt.Errorf("reading binary.gz for verification: %v", err)
	} else if sum := "0" + base64.RawURLEncoding.EncodeToString(h.Sum(nil)[:20]); sum != record.Sum {
		return -1, fmt.Errorf("latest binary.gz sum mismatch, got %s, expect %s", sum, record.Sum)
	} else if err := f.Close(); err != nil {
		return -1, fmt.Errorf("close binary.gz: %v", err)
	}
	return numRecords, nil
}

func parseRemoteAddr(addr string) netip.Addr {
	ipstr, _, err := net.SplitHostPort(addr)
	if err != nil {
		slog.Info("parsing remote address", "err", err, "addr", addr)
		return netip.Addr{}
	}
	ip, err := netip.ParseAddr(ipstr)
	if err != nil {
		slog.Info("parsing ip from remote address", "err", err, "addr", addr, "ipstr", ipstr)
		return netip.Addr{}
	}
	return ip
}

type httpRemoteAddrHandler struct {
	h http.Handler
}

// keyRemoteAddr is the key for a context value with the remote address of an http
// request.
type keyRemoteAddr struct{}

func (h httpRemoteAddrHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := context.WithValue(r.Context(), keyRemoteAddr{}, r.RemoteAddr)
	h.h.ServeHTTP(w, r.WithContext(ctx))
}
