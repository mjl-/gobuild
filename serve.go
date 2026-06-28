package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	_ "embed"
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
	_ "net/http/pprof"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mjl-/gobuild/internal/sumdb"
	"github.com/mjl-/gobuild/ratelimit"

	"github.com/mjl-/bstore"
	"github.com/mjl-/sconf"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/mod/sumdb/note"
	"golang.org/x/mod/sumdb/tlog"
)

var (
	database *bstore.DB

	// Set to absolute paths: the config file can have relative paths.
	workdir    string
	homedir    string
	latestPath string
	commandDir string

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
		"",
		nil,
		"",
		0,
		Ratelimit{false},
		5 * time.Second,
		0,
		&slog.LevelVar{},
	}
	emptyConfig = config

	// Either separate log file or nil, append-only logging of added sums.
	sumLogFile io.Writer

	trustedProxyIPs []netip.Prefix

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
	HomeDir      string     `sconf-doc:"Directory used for builds. The cmds subdirectory will get temporary directories for builds. Previously, gobuild stored the Go caches, and downloaded and extracted modules in the home directory directly."`
	MaxBuilds    int        `sconf-doc:"Maximum concurrent builds. Default (0) uses NumCPU+1."`
	Environment  []string   `sconf:"optional" sconf-doc:"Additional environment variables in form KEY=VALUE to use for go command invocations. Useful to configure GOSUMDB and HTTPS_PROXY."`
	Run          []string   `sconf:"optional" sconf-doc:"Command and parameters to prefix invocations of go with. For example /usr/bin/nice."`
	BuildGobin   bool       `sconf-doc:"If enabled, sets environment variable GOBUILD_GOBIN during a build to a directory where the build command should write the binary. Configure a wrapper to the build command through the Run config option that writes to this directory. Deprecated: No longer needed since each build is executed in its own temporary directory."`
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
	LogDir                       string          `sconf-doc:"Directory to store log files. HTTP access logs are written, one file per day. Additions to the transparency logs are written to sum.log. Leave empty to disable logging."`
	ModulePrefixes               []string        `sconf:"optional" sconf-doc:"If non-empty, allow list of module prefixes for which binaries will be built. Requests for other module prefixes result in an error. Prefixes should typically end with a slash."`
	SDKVersionOldest             string          `sconf:"optional" sconf-doc:"If set, the oldest Go toolchain version that we will builds for."`
	SDKVersionStop               string          `sconf:"optional" sconf-doc:"If set, the (hypothetical) version (and beyond) of the Go toolchain that is not allowed for builds. Gobuild automatically downloads new SDKs. However, new Go toolchain versions may change behaviour which may cause binaries to no longer become reproducible with the flags gobuild uses to build. By refusing new versions, you have time to separately verify binaries with newer Go toolchains are still reproducible. Example: a version of go1.20 allows go1.18, go1.19, go1.19.1, but not go1.20, go1.21 or go2.0. Versions like go1.20rc1 are interpreted as go1.20, without rc1."`
	InstanceNotesFile            string          `sconf:"optional" sconf-doc:"If set, a path to a plain text file with notes about this gobuild instance that is included on the main page."`
	BadClients                   []ClientPattern `sconf:"optional" sconf-doc:"Clients for which we won't start a new build. To prevent bad bots that ignore robots.txt from causing lots of builds."`
	BinaryCacheSizeMax           string          `sconf:"optional" sconf-doc:"Maximum size of the cache with built binaries. When adding a new binary would go over the size, binaries with the oldest access time are removed to stay under 90% of the maximum size. Value must end in MB or GB. The actual total size occupied by the binaries can temporarily exceed the configured maximum size. Can not be used with CleanupBinariesAccessTimeAge."`
	CleanupBinariesAccessTimeAge time.Duration   `sconf:"optional" sconf-doc:"Remove build result binaries with an access time longer this duration ago, if > 0. Binaries will be rebuilt, and verified to match the expected sum, when requested again. Can not be used with BinaryCacheSizeMax."`
	Ratelimit                    Ratelimit       `sconf:"optional" sconf-doc:"Requests for builds (compilation) through the web pages can be rate limited to prevent overloading the server. The lookups through the /tlog endpoints are not affected."`
	ShutdownTimeout              time.Duration   `sconf:"optional" sconf-doc:"Maximum time to wait for all connections to finish during shutdown."`

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

	prefixes []netip.Prefix
}

var startTime = time.Now()

// Matches returns if a request matches the pattern.
func (cp ClientPattern) Match(r *http.Request) (hostname string, match bool) {
	if cp.UserAgent != "" {
		if !strings.Contains(strings.ToLower(r.UserAgent()), cp.UserAgent) {
			return "", false
		}
	}
	if len(cp.prefixes) == 0 && cp.HostnameSuffix == "" {
		return "", false
	}
	ip := remoteIP(r)
	if len(cp.prefixes) > 0 {
		found := false
		for _, prefix := range cp.prefixes {
			if prefix.Contains(ip) {
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
		ipstr := net.IP(ip.AsSlice()).String()
		hosts, err := net.DefaultResolver.LookupAddr(ctx, ipstr)
		if err != nil {
			logger(r.Context()).Debug("reverse lookup for ip", "ip", ipstr, "err", err)
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

type Result struct {
	ID int64

	// See type buildSpec.
	Mod       string `bstore:"unique Mod+Version+Dir+Goos+Goarch+Goversion+Stripped"` // E.g. github.com/mjl-/gobuild. Never starts or ends with slash, and is never empty.
	Version   string
	Dir       string // Always starts with slash. Never ends with slash unless "/".
	Goos      string
	Goarch    string
	Goversion string
	Stripped  bool

	Created time.Time `bstore:"default now"`

	ErrorReason string // If "unknown", we don't know why this failed. Otherwise a short text about the reason.

	FileSizeGz int64 // Can be 0 if for historic builds. Also see [TreeRecord.FileSize].

	TreeRecordID ID // 0 if not valid
}

func (r Result) buildSpec() buildSpec {
	return buildSpec{
		Mod:       r.Mod,
		Version:   r.Version,
		Dir:       r.Dir,
		Goos:      r.Goos,
		Goarch:    r.Goarch,
		Goversion: r.Goversion,
		Stripped:  r.Stripped,
	}
}

// BuildLog is the output of the go build command.
type BuildLog struct {
	ID   int64  `bstore:"ref Result"`
	Data []byte // Gzipped.
}

// ID is an identifier for a TreeRecord or TreeHash stored in the database, with
// values starting at 1. The corresponding number as used in tlogs start at 0.
type ID int64

// Number is an identifier for a record or hash stored in a tlog, with values
// starting at 0. The corresponding database id's start at 1.
type Number int64

// Number returns the tree record number (index) based on the ID.
// IDs start counting at 1, numbers start counting at 0.
func (r ID) Number() Number {
	return Number(int64(r) - 1)
}

func (r Number) ID() ID {
	return ID(int64(r) + 1)
}

// TreeRecord is a record for a tlog.
type TreeRecord struct {
	ID       ID
	FileSize int64
	Sum      buildSum
	ResultID int64 `bstore:"nonzero,ref Result"`
}

// TreeHash is a hash that is part of a tlog, one or more is added when adding
// a record to to a tlog.
type TreeHash struct {
	ID  ID
	Sum tlog.Hash
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

	//go:embed template/admin.html
	adminHTML []byte

	// For waiting until all http servers and goroutines are shutdown.
	wgShutdown sync.WaitGroup

	transactionID = new(atomic.Int64)
)

func init() {
	transactionID.Store(time.Now().UnixMilli())
}

type ctxKeyLog struct{} // For context value with an slog.Logger

func logger(ctx context.Context) *slog.Logger {
	v := ctx.Value(ctxKeyLog{})
	if v == nil {
		return slog.Default()
	}
	return v.(*slog.Logger)
}

// tidFmt implements slog.LogValuer.
type tidFmt int64

func (v tidFmt) LogValue() slog.Value {
	return slog.StringValue(fmt.Sprintf("%x", v))
}

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
	trustedProxyIPsStr := serveFlags.String("trusted-proxy-ips", "", "comma-separated list of ip networks from which x-forwarded-for http headers are trusted and used for ip rate limiting and logging, eg '127.0.0.0/8,::/96'")

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
	slogger := slog.New(logHandler)
	slog.SetDefault(slogger)

	_, errGcc := exec.Command("which", "gcc").CombinedOutput()
	_, errClang := exec.Command("which", "clang").CombinedOutput()
	if errGcc == nil {
		slog.Error("gcc must not be installed (in $PATH) to prevent interference with builds")
	}
	if errClang == nil {
		slog.Error("clang must not be installed (in $PATH) to prevent interference with builds")
	}
	if errGcc == nil || errClang == nil {
		os.Exit(1)
	}

	for s := range strings.SplitSeq(*trustedProxyIPsStr, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(s)
		if err != nil {
			log.Fatalf("parsing ip network %q for trusted proxy ips: %v", s, err)
		}
		trustedProxyIPs = append(trustedProxyIPs, prefix)
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
	if config.SDKVersionOldest != "" {
		v, err := parseGoVersion(config.SDKVersionOldest)
		if err != nil {
			log.Fatalf("parsing config.SDKVersionOldest %q from config: %s", config.SDKVersionOldest, err)
		}
		sdkVersionOldest = &v
	}
	if config.SDKVersionStop != "" {
		v, err := parseGoVersion(config.SDKVersionStop)
		if err != nil {
			log.Fatalf("parsing SDKVersionStop %q from config: %s", config.SDKVersionStop, err)
		}
		sdkVersionStop = &v
	}

	if buildInfo, ok := debug.ReadBuildInfo(); ok {
		gobuildVersion = buildInfo.Main.Version
	}
	gobuildVersion += " " + runtime.Version()

	// Once we get a signal, we stop accepting new connections.
	acceptCtx, acceptCancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer acceptCancel()

	// HTTP requests use a base context we cancel after the graceful shutdown timeout.
	// After which we wait one more second until final shutdown.
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	defer shutdownCancel()

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
	latestPath = filepath.Join(homedir, "sumdb-latest")
	commandDir = filepath.Join(homedir, "cmds")

	// Remove any existing commandDir, if the service wasn't properly shut down, there
	// may be temp files left. We don't want to accumulate old files.
	chmodRecursive(shutdownCtx, commandDir) // For pkg/go/mod files that may be read-only.
	if err := os.RemoveAll(commandDir); err != nil {
		slog.Error("removing old commandDir with potential leftover temporary files, continuing", "err", err)
	}
	// failures will be caught later
	os.Mkdir(commandDir, 0777)
	os.MkdirAll(config.SDKDir, 0777) // may already exist, we'll get errors later

	os.MkdirAll(filepath.Join(config.DataDir, "binaries"), 0777)

	// If $datadir/result exists, and $datadir/gobuild.db does not, we'll migrate.
	needMigrate := func() (bool, error) {
		_, err := os.Stat(filepath.Join(config.DataDir, "sum", "hashes"))
		if err != nil && errors.Is(err, fs.ErrNotExist) {
			return false, nil
		} else if err != nil {
			return false, fmt.Errorf("stat $datadir/sum/hashes: %w", err)
		}
		_, err = os.Stat(filepath.Join(config.DataDir, "gobuild.db"))
		if err == nil {
			return false, nil
		} else if errors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		return false, fmt.Errorf("stat $datadir/gobuild.db: %w", err)
	}
	if need, err := needMigrate(); err != nil {
		slog.Error("checking whether to migrate to database", "err", err)
		os.Exit(1)
	} else if need {
		slog.Info("migrating data directory to bstore database")
		start := time.Now()
		if err := migrateToDB(acceptCtx); err != nil {
			slog.Error("migrating to database", "err", err)
			os.Exit(1)
		}
		slog.Info("migration completed", "duration", time.Since(start))
	} else {
		mustOpenDB(acceptCtx)
	}

	// Verify the most recent additions to the records & hashes files are consistent.
	if recordCount, err := verifySumState(shutdownCtx); err != nil {
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

	readRecentBuilds(shutdownCtx)

	go func() {
		// If coordinateBuilds panics, we will grind to a halt, but at least we'll get alerting about it.
		defer logPanic(slog.Default())

		coordinateBuilds(shutdownCtx)
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

		// Walk data's binaries/ dir to gather the current size. We won't remove anything.
		if err := binaryCacheCleanup(0); err != nil {
			slog.Error("determining current size of binaries cache", "err", err)
		}

		// We may need to cleanup now.
		if reclaim := binaryCacheSizeAdd(0); reclaim > 0 {
			go func() {
				defer logPanic(slog.Default())

				err := binaryCacheCleanup(reclaim)
				if err != nil {
					slog.Error("cleaning up binary cache at startup", "err", err, "reclaim", reclaim)
				}
			}()
		}
	}
	if config.CleanupBinariesAccessTimeAge > 0 {
		go func() {
			defer logPanic(slog.Default())
			time.Sleep(time.Minute)
			for {
				cleanupBinariesAtime(config.CleanupBinariesAccessTimeAge)
				time.Sleep(24 * time.Hour)
			}
		}()
	}

	reg := registerGobuildMetrics()
	http.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	http.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(adminHTML)
	})
	http.HandleFunc("GET /pending", func(w http.ResponseWriter, r *http.Request) {
		// Return list of pending builds, including IP addresses.
		rc := make(chan coordinatorState)
		coordinate.state <- rc
		state := <-rc

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		type elem struct {
			bs        buildSpec
			added     time.Time
			started   *time.Time
			initiator netip.Addr
		}
		var l []elem
		for bs, b := range state.builds {
			l = append(l, elem{bs, b.added, b.started, b.initiator})
		}
		slices.SortFunc(l, func(a, b elem) int {
			if a.added.Before(b.added) {
				return -1
			}
			return 1
		})
		fmt.Fprintf(w, "# buildspec, added, started, initiator\n")
		for _, e := range l {
			fmt.Fprintf(w, "%s\t%v\t%v\t%s\n", e.bs.String(), e.added, e.started, e.initiator.String())
		}
	})
	http.HandleFunc("GET /buildfailures", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")

		bw := bufio.NewWriter(w)
		fmt.Fprintf(bw, "# id, buildspec: reason (created)\n")

		q := bstore.QueryDB[Result](r.Context(), database)
		q.FilterEqual("TreeRecordID", ID(0))
		q.SortDesc("Created")
		for result, err := range q.All() {
			if err != nil {
				http.Error(w, "500 - internal server error - "+err.Error(), http.StatusInternalServerError)
				return
			}
			_, err := fmt.Fprintf(bw, "%d, %s: %q (%s)\n", result.ID, result.buildSpec(), result.ErrorReason, result.Created)
			logCheck(r.Context(), err, "write build failure")
			if err != nil {
				break
			}
		}

		bw.Flush()
	})
	http.HandleFunc("GET /db", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		w.Header().Set("Content-Type", "application/octet-stream")
		err := database.Read(ctx, func(tx *bstore.Tx) error {
			_, err := tx.WriteTo(w)
			return err
		})
		logCheck(ctx, err, "write database dump")
	})
	http.HandleFunc("GET /results", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		bw := bufio.NewWriter(w)
		fmt.Fprintf(bw, "id, record id, link, reason, created, size, sizegz\n")

		ctx := r.Context()

		err := database.Read(ctx, func(tx *bstore.Tx) error {
			q := bstore.QueryTx[Result](tx)
			q.SortAsc("ID")
			for result, err := range q.All() {
				if err != nil {
					return err
				}
				var record TreeRecord
				link := "/" + result.buildSpec().String()
				if result.TreeRecordID > 0 {
					record = TreeRecord{ID: result.TreeRecordID}
					if err := tx.Get(&record); err != nil {
						fmt.Fprintf(bw, "error: get record: %v\n", err)
					} else {
						link += record.Sum.String() + "/"
					}
				}
				_, err := fmt.Fprintf(bw, "%d, %d, %s, %q, %s, %d, %d\n", result.ID, result.TreeRecordID, link, result.ErrorReason, result.Created, record.FileSize, result.FileSizeGz)
				if err != nil {
					return err
				}
			}
			return nil
		})
		logCheck(ctx, err, "get results")
		bw.Flush()
	})
	clearFailed := func(w http.ResponseWriter, r *http.Request, sinceID int64) {
		err := database.Write(r.Context(), func(tx *bstore.Tx) error {
			q := bstore.QueryTx[Result](tx)
			q.FilterEqual("TreeRecordID", ID(0))
			q.FilterGreaterEqual("ID", ID(sinceID))
			for result, err := range q.All() {
				if err != nil {
					return err
				}
				bl := BuildLog{ID: result.ID}
				if err := tx.Delete(&bl, &result); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			http.Error(w, "500 - internal server error - deleting failed builds: "+err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
	http.HandleFunc("POST /clearall", func(w http.ResponseWriter, r *http.Request) {
		clearFailed(w, r, 0)
	})
	http.HandleFunc("POST /clear", func(w http.ResponseWriter, r *http.Request) {
		idStr := r.FormValue("id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			http.Error(w, fmt.Sprintf("400 - bad request - parsing form value id %q as int: %v", idStr, err), http.StatusBadRequest)
			return
		}
		clearFailed(w, r, id)
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(fileFaviconPng) // nothing to do for errors
	})
	mux.HandleFunc("GET /favicon-building.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(fileFaviconBuildingPng) // nothing to do for errors
	})
	mux.HandleFunc("GET /favicon-error.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(fileFaviconErrorPng) // nothing to do for errors
	})

	mux.HandleFunc("GET /emptyconfig", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		sconf.Describe(w, &emptyConfig) // nothing to do for errors
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

		h := httpRequestHandler{http.StripPrefix("/tlog", sumdb.NewServer(serverOps{signer}))}
		for _, path := range sumdb.ServerPaths {
			mux.Handle("GET /tlog"+path, h)
		}
	}

	mux.HandleFunc("GET /img/gopher-dance-long.gif", func(w http.ResponseWriter, r *http.Request) {
		defer observePage("dance", time.Now())
		w.Header().Set("Content-Type", "image/gif")
		w.Write(fileGopherDanceLongGif) // nothing to do for errors
	})

	// These prefixes are old. We still serve on these paths for compatibility.
	mux.HandleFunc("GET /m/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path[2:], http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("GET /b/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path[2:], http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("GET /r/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path[2:], http.StatusTemporaryRedirect)
	})

	mux.HandleFunc("GET /{$}", serveHome)
	mux.HandleFunc("GET /", serveSpec)
	mux.HandleFunc("POST /", serveSpec)

	var handler http.Handler = mux
	if config.LogDir != "" {
		os.MkdirAll(config.LogDir, 0777)
		handler = newLogHandler(mux, config.LogDir)

		sumLogFile, err = os.OpenFile(filepath.Join(config.LogDir, "sum.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0666)
		if err != nil {
			log.Fatalf("open sum.log: %v", err)
		}
	}

	var httpsaddr string
	if config.HTTPS != nil {
		httpsaddr = ":443"
	}
	slog.Info("starting gobuild", "httpaddr", *listenHTTP, "httpsaddr", httpsaddr, "adminaddr", *listenAdmin, "version", gobuildVersion, "goversion", runtime.Version(), "goos", runtime.GOOS, "goarch", runtime.GOARCH)

	var servers []*http.Server // For calling Shutdown after the signal.

	// ctxlogHandler passes the ServeHTTP on to h, but with ctx replaced with one that has
	// a ctxKeyLog as context value.
	ctxlogHandler := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tid := transactionID.Add(1)
			log := slog.With("tid", tidFmt(tid), "remoteip", remoteIP(r))
			ctx := context.WithValue(r.Context(), ctxKeyLog{}, log)
			h.ServeHTTP(w, r.WithContext(ctx))
		})
	}

	if *listenHTTP != "" {
		server := &http.Server{
			Addr: *listenHTTP,
			BaseContext: func(net.Listener) context.Context {
				return shutdownCtx
			},
			Handler: ctxlogHandler(handler),

			ReadHeaderTimeout: 30 * time.Second,
			ReadTimeout:       5 * time.Minute,
			WriteTimeout:      30 * time.Minute,
			IdleTimeout:       120 * time.Second,
		}
		servers = append(servers, server)
		wgShutdown.Go(func() {
			err := server.ListenAndServe()
			if err == http.ErrServerClosed {
				slog.Info("listen and serve on http", "err", err)
			} else {
				slog.Error("listen and serve on http", "err", err)
				os.Exit(1)
			}
		})
	}
	if config.HTTPS != nil {
		os.MkdirAll(config.HTTPS.ACME.CertDir, 0700) // errors will come up later
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(config.HTTPS.ACME.Domains...),
			Cache:      autocert.DirCache(config.HTTPS.ACME.CertDir),
			Email:      config.HTTPS.ACME.Email,
		}
		server := &http.Server{
			BaseContext: func(net.Listener) context.Context {
				return shutdownCtx
			},
			Handler: ctxlogHandler(handler),

			ReadHeaderTimeout: 30 * time.Second,
			ReadTimeout:       5 * time.Minute,
			WriteTimeout:      30 * time.Minute,
			IdleTimeout:       120 * time.Second,
		}
		servers = append(servers, server)
		wgShutdown.Go(func() {
			err := server.Serve(m.Listener())
			if err == http.ErrServerClosed {
				slog.Info("listen and serve on https", "err", err)
			} else {
				slog.Error("listen and serve on https", "err", err)
				os.Exit(1)
			}
		})
	}
	if *listenAdmin != "" {
		server := &http.Server{
			Addr: *listenAdmin,
			BaseContext: func(net.Listener) context.Context {
				return shutdownCtx
			},
			Handler: ctxlogHandler(http.DefaultServeMux),

			ReadHeaderTimeout: 30 * time.Second,
			ReadTimeout:       5 * time.Minute,
			WriteTimeout:      30 * time.Minute,
			IdleTimeout:       120 * time.Second,
		}
		servers = append(servers, server)
		wgShutdown.Go(func() {
			err := server.ListenAndServe()
			if err == http.ErrServerClosed {
				slog.Info("listen and serve on https", "err", err)
			} else {
				slog.Error("listen and serve on admin", "err", err)
				os.Exit(1)
			}
		})
	}

	// When shutting down, make sure no modifications to transparency log are in progress.
	<-acceptCtx.Done()
	slog.Warn("shutdown after sigint or sigterm, waiting for connections/requests to finish", "timeout", config.ShutdownTimeout)
	acceptCancel() // Ensure next ctrl-c stops immediately.

	// Shutdown webservers and other goroutines cleanly.
	stopCtx, stopCancel := context.WithTimeout(shutdownCtx, config.ShutdownTimeout)
	defer stopCancel()

	for _, s := range servers {
		wgShutdown.Go(func() {
			if err := s.Shutdown(stopCtx); err != nil {
				slog.Error("shutting down http server", "err", err)
			}
		})
	}
	// Wait for http servers and select goroutines (e.g. for builds) to finish.
	wgShutdown.Wait()

	slog.Info("http servers and builds shut down, stopping other active operations and waiting 1s")
	shutdownCancel()
	time.Sleep(time.Second)

	slog.Info("exiting... (after any active sumdb add has finished)")
	addSumMutex.Lock()
}

func mustOpenDB(ctx context.Context) {
	p := filepath.Join(config.DataDir, "gobuild.db")
	var err error
	database, err = bstore.Open(ctx, p, &bstore.Options{Timeout: 5 * time.Second}, Result{}, BuildLog{}, TreeRecord{}, TreeHash{})
	if err != nil {
		slog.Error("open database", "err", err, "path", p)
		os.Exit(1)
	}
}

func logPanic(logger *slog.Logger) {
	x := recover()
	if x == nil {
		return
	}

	metricPanics.Inc()
	logger.Error("unhandled panic", "panic", x)
	debug.PrintStack()
}

func logCheck(ctx context.Context, err error, msg string, args ...any) {
	if err != nil {
		args = append([]any{"err", err}, args...)
		logger(ctx).Error(msg, args...)
	}
}

func failf(w http.ResponseWriter, r *http.Request, format string, args ...any) {
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
	statusfail(r.Context(), status, w, errmsg)
}

func statusfail(ctx context.Context, status int, w http.ResponseWriter, errmsg string) {
	msg := fmt.Sprintf("%d - %s - %s", status, http.StatusText(status), errmsg)
	if status/100 == 5 {
		metricHTTPRequestsServerErrors.Inc()
		logger(ctx).Error("http server error", "status", status, "err", errmsg)
		debug.PrintStack()
	} else {
		metricHTTPRequestsUserErrors.Inc()
		logger(ctx).Debug("http user error", "status", status, "err", errmsg)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	err := errorTemplate.Execute(w, map[string]string{"Message": msg})
	if err != nil && !(isClosed(err) || strings.HasSuffix(errmsg, "http2: stream closed") || strings.HasSuffix(errmsg, "client disconnected")) {
		logger(ctx).Error("executing template for error", "err", err)
	}
}

func serveLog(w http.ResponseWriter, r *http.Request, resultID int64) {
	bl := BuildLog{ID: resultID}
	if err := database.Get(r.Context(), &bl); err != nil {
		failf(w, r, "%w: get build log: %v", errServer, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	serveGzipFile(w, r, bytes.NewReader(bl.Data))
}

func serveGzipFile(w http.ResponseWriter, r *http.Request, src io.Reader) {
	if acceptsGzip(r) {
		w.Header().Set("Content-Encoding", "gzip")
		io.Copy(w, src) // nothing to do for errors
	} else if gzr, err := gzip.NewReader(src); err != nil {
		failf(w, r, "%w: decompressing: %s", errServer, err)
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

func verifySumState(ctx context.Context) (treeSize int64, rerr error) {
	var record TreeRecord
	var result Result

	err := database.Read(ctx, func(tx *bstore.Tx) error {
		recordCount, err := bstore.QueryTx[TreeRecord](tx).Count()
		if err != nil {
			return fmt.Errorf("get tree size: %w", err)
		}
		treeSize = int64(recordCount)

		hashCount, err := bstore.QueryTx[TreeHash](tx).Count()
		if err != nil {
			return fmt.Errorf("get tree size: %w", err)
		}

		if expHashCount := tlog.StoredHashCount(int64(recordCount)); int64(hashCount) != expHashCount {
			return fmt.Errorf("got %d hashes for %d records, expected %d hashes", hashCount, recordCount, expHashCount)
		}

		if recordCount == 0 {
			return nil
		}

		record = TreeRecord{ID: ID(recordCount)}
		if err := tx.Get(&record); err != nil {
			return fmt.Errorf("get record: %v", err)
		}
		result = Result{ID: record.ResultID}
		if err := tx.Get(&result); err != nil {
			return fmt.Errorf("get result: %v", err)
		}

		records, err := serverOps{}.ReadRecords(ctx, int64(record.ID.Number()), 1)
		if err != nil {
			return fmt.Errorf("reading last record: %v", err)
		}
		expHashes, err := tlog.StoredHashes(int64(record.ID.Number()), records[0], hashReader{tx})
		if err != nil {
			return fmt.Errorf("calculating hashes for most recent record: %v", err)
		}
		hashNum := tlog.StoredHashIndex(0, int64(record.ID.Number()))
		for i, eh := range expHashes {
			hashes, err := hashReader{tx}.ReadHashes([]int64{hashNum + int64(i)})
			if err != nil {
				return fmt.Errorf("read hashes for most recent record: %v", err)
			}
			if eh != hashes[0] {
				return fmt.Errorf("hash %d+%d mismatch for last record %d, got %x, expect %x", hashNum, i, record.ID, hashes[0], eh)
			}
		}

		return nil
	})
	if err != nil {
		return -1, fmt.Errorf("verifying last record and hashes: %v", err)
	}

	if treeSize == 0 {
		return 0, nil
	}

	// And check if the hash of the binary matches the sum.
	p := filepath.Join(config.DataDir, "binaries", fmt.Sprintf("%d.gz", record.ID))
	f, err := os.Open(p)
	if err != nil && errors.Is(err, fs.ErrNotExist) {
		return treeSize, nil
	} else if err != nil {
		return -1, fmt.Errorf("open binary for verification: %v", err)
	}
	defer f.Close()
	h := sha256.New()
	if gzr, err := gzip.NewReader(f); err != nil {
		return -1, fmt.Errorf("gzip reader for binary: %v", err)
	} else if _, err := io.Copy(h, gzr); err != nil {
		return -1, fmt.Errorf("reading binary for verification: %v", err)
	} else if sum := (buildSum{[20]byte(h.Sum(nil)[:20])}); sum != record.Sum {
		return -1, fmt.Errorf("latest binary sum mismatch, got %x, expect %x", sum, record.Sum)
	}
	return treeSize, nil
}

// remoteIP returns the remote ip for the request, based on X-Forwarded-For
// headers combined with trusted proxy ips, or otherwise the request's RemoteAddr.
func remoteIP(r *http.Request) netip.Addr {
	ipstr, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		logger(r.Context()).Info("parsing remote address", "err", err, "addr", r.RemoteAddr)
		return netip.Addr{}
	}

	if l := r.Header.Values("X-Forwarded-For"); len(trustedProxyIPs) > 0 && len(l) > 0 {
		l = strings.Split(strings.Join(l, ","), ",")
		l = append(l, ipstr)

		var last netip.Addr
	Address:
		for i := len(l) - 1; i >= 0; i-- {
			l[i] = strings.TrimSpace(l[i])
			host, _, err := net.SplitHostPort(l[i])
			if err != nil {
				host = l[i]
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				logger(r.Context()).Info("parsing ip from x-forwarded-for, stopping processing", "err", err, "addr", l[i], "xff", strings.Join(r.Header.Values("X-Forwarded-For"), ","))
				break
			}
			last = ip
			for _, prefix := range trustedProxyIPs {
				if prefix.Contains(ip) {
					continue Address
				}
			}
			break
		}
		if last.IsValid() {
			return last
		}
	}

	ip, err := netip.ParseAddr(ipstr)
	if err != nil {
		logger(r.Context()).Info("parsing ip from remote address", "err", err, "addr", r.RemoteAddr, "ipstr", ipstr)
		return netip.Addr{}
	}
	return ip
}

type httpRequestHandler struct {
	h http.Handler
}

// keyRequest is the key for a context value with the http request.
type keyRequest struct{}

func (h httpRequestHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := context.WithValue(r.Context(), keyRequest{}, r)
	h.h.ServeHTTP(w, r.WithContext(ctx))
}
