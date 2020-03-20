// Gobuild serves reproducibly built binaries from sources via go module proxy.
//
// Serves URLs like:
//
// 	http://localhost:8000/
// 	http://localhost:8000/x/linux-amd64-go1.14.1/github.com/mjl-/sherpa@v0.6.0/cmd/sherpaclient/{,log,sha256,dl}
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	listenAddress = flag.String("listen", "localhost:8000", "address to serve on")
	listenAdmin   = flag.String("listenadmin", "localhost:8001", "address to serve admin-related http on")
	workdir       string

	recentBuilds struct {
		sync.Mutex
		paths []string
	}

	// We keep a map of available builds, so we can show in links in UI that navigating
	// won't trigger a build but will return quickly.
	availableBuilds = struct {
		sync.Mutex
		index map[string]struct{} // keys are: goos-goarch-goversion/mod@version/dir
	}{
		sync.Mutex{},
		map[string]struct{}{},
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
		log.Println("usage: gobuild [flags]")
		flag.PrintDefaults()
	}
	flag.Parse()
	args := flag.Args()
	if len(args) != 0 {
		flag.Usage()
		os.Exit(2)
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
	mux.HandleFunc("/x/", serve)
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

	b := &bytes.Buffer{}
	err := homeTemplate.Execute(b, recents)
	if err != nil {
		failf(w, "executing template: %v", err)
	}
	writeHTML(w, b.Bytes())
}

type page int

const (
	pageIndex page = iota
	pageLog
	pageSha256
	pageDownloadRedirect
	pageDownload
)

var pageNames = []string{"index", "log", "sha256", "dlredir", "download"}

func (p page) String() string {
	return pageNames[p]
}

type request struct {
	Mod         string
	Version     string
	Dir         string // Either empty, or ending with a slash.
	Goos        string
	Goarch      string
	Goversion   string
	Page        page
	DownloadSum string
}

func (r request) destdir() string {
	return fmt.Sprintf("%s-%s-%s/%s@%s/%s", r.Goos, r.Goarch, r.Goversion, r.Mod, r.Version, r.Dir)
}

// name of file the http user-agent (browser) will save the file as.
func (r request) downloadFilename() string {
	name := path.Base(r.Dir)
	if name == "" {
		name = path.Base(r.Mod)
	}
	ext := ""
	if r.Goos == "windows" {
		ext = ".exe"
	}
	return fmt.Sprintf("%s-%s-%s%s", name, r.Version, r.Goversion, ext)
}

// we'll get paths like linux-amd64-go1.13/example.com/user/repo@version/cmd/dir/{log,sha256,dl,<sum>}
func parsePath(s string) (r request, ok bool) {
	t := strings.SplitN(s, "/", 2)
	if len(t) != 2 {
		return
	}
	s = t[1]
	tt := strings.Split(t[0], "-")
	if len(tt) != 3 {
		return
	}
	r.Goos = tt[0]
	r.Goarch = tt[1]
	r.Goversion = tt[2]

	switch {
	case strings.HasSuffix(s, "/"):
		r.Page = pageIndex
		s = s[:len(s)-len("/")]
	case strings.HasSuffix(s, "/log"):
		r.Page = pageLog
		s = s[:len(s)-len("/log")]
	case strings.HasSuffix(s, "sha256"):
		r.Page = pageSha256
		s = s[:len(s)-len("/sha256")]
	case strings.HasSuffix(s, "/dl"):
		r.Page = pageDownloadRedirect
		s = s[:len(s)-len("/dl")]
	case len(s) >= 1+40 && s[len(s)-1-40] == '/':
		r.Page = pageDownload
		r.DownloadSum = s[len(s)-40:]
		s = s[:len(s)-1-40]
	default:
		return
	}

	// We are left parsing eg:
	// - example.com/user/repo@version/cmd/dir
	// - example.com/user/repo@version
	t = strings.SplitN(s, "@", 2)
	if len(t) != 2 {
		return
	}
	r.Mod = t[0]
	s = t[1]
	t = strings.SplitN(s, "/", 2)
	r.Version = t[0]
	if len(t) == 2 {
		r.Dir = t[1] + "/"
	}

	ok = true
	return
}

func failf(w http.ResponseWriter, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	log.Println(msg)
	http.Error(w, "500 - "+msg, http.StatusInternalServerError)
}

func serve(w http.ResponseWriter, r *http.Request) {
	req, ok := parsePath(r.URL.Path[3:])
	if !ok {
		http.NotFound(w, r)
		return
	}

	metricRequestsTotal.WithLabelValues(req.Page.String())

	lpath := "data/" + req.destdir()
	_, err := os.Stat(lpath)
	if err != nil {
		if !os.IsNotExist(err) {
			failf(w, "stat path: %v", err)
			return
		}
		metricBuilds.WithLabelValues(req.Goos, req.Goarch, req.Goversion).Inc()
		ok := build(w, r, req)
		if !ok {
			metricBuildErrors.WithLabelValues(req.Goos, req.Goarch, req.Goversion).Inc()
			return
		}
		p := fmt.Sprintf("%s-%s-%s/%s@%s/%s", req.Goos, req.Goarch, req.Goversion, req.Mod, req.Version, req.Dir)
		recentBuilds.Lock()
		recentBuilds.paths = append(recentBuilds.paths, p)
		if len(recentBuilds.paths) > 10 {
			recentBuilds.paths = recentBuilds.paths[len(recentBuilds.paths)-10:]
		}
		recentBuilds.Unlock()
		availableBuilds.Lock()
		availableBuilds.index[p] = struct{}{}
		availableBuilds.Unlock()
	}

	switch req.Page {
	case pageLog:
		p := lpath + "/log.gz"
		f, err := os.Open(p)
		if err != nil {
			failf(w, "open log.gz: %v", err)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		serveGzipFile(w, r, p, f)
	case pageSha256:
		f, err := os.Open(lpath + "/sha256")
		if err != nil {
			failf(w, "open log: %v", err)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.Copy(w, f)
	case pageDownloadRedirect:
		buf, err := ioutil.ReadFile(lpath + "/sha256")
		if err != nil {
			failf(w, "open sha256: %v", err)
			return
		}
		if len(buf) != 64 {
			failf(w, "bad sha256 file")
			return
		}
		sum := string(buf[:40])
		http.Redirect(w, r, sum, http.StatusFound)

	case pageDownload:
		p := lpath + "/" + req.DownloadSum + ".gz"
		f, err := os.Open(p)
		if err != nil {
			if os.IsNotExist(err) {
				http.NotFound(w, r)
			} else {
				failf(w, "open binary: %v", err)
			}
			return
		}
		defer f.Close()
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", req.downloadFilename()))
		serveGzipFile(w, r, p, f)
	case pageIndex:
		buf, err := ioutil.ReadFile(lpath + "/sha256")
		if err != nil {
			failf(w, "reading sha256: %v", err)
			return
		}
		if len(buf) != 64 {
			failf(w, "bad sha256 file")
		}
		sum := string(buf[:40])

		type goversionLink struct {
			Goversion string
			Path      string
			Available bool
		}
		goversionLinks := []goversionLink{}
		for _, goversion := range installedSDK() {
			p := fmt.Sprintf("%s-%s-%s/%s@%s/%s", req.Goos, req.Goarch, goversion, req.Mod, req.Version, req.Dir)
			goversionLinks = append(goversionLinks, goversionLink{goversion, p, false})
		}

		type targetLink struct {
			Goos      string
			Goarch    string
			Path      string
			Available bool
		}
		targetLinks := []targetLink{}
		for _, target := range targets {
			p := fmt.Sprintf("%s-%s-%s/%s@%s/%s", target.Goos, target.Goarch, req.Goversion, req.Mod, req.Version, req.Dir)
			targetLinks = append(targetLinks, targetLink{target.Goos, target.Goarch, p, false})
		}

		availableBuilds.Lock()
		for i, link := range goversionLinks {
			_, ok := availableBuilds.index[link.Path]
			goversionLinks[i].Available = ok
		}
		for i, link := range targetLinks {
			_, ok := availableBuilds.index[link.Path]
			targetLinks[i].Available = ok
		}
		availableBuilds.Unlock()

		args := map[string]interface{}{
			"Req":            req,
			"Sum":            sum,
			"GoversionLinks": goversionLinks,
			"TargetLinks":    targetLinks,
		}
		b := &bytes.Buffer{}
		err = buildTemplate.Execute(b, args)
		if err != nil {
			failf(w, "executing html template: %v", err)
			return
		}
		writeHTML(w, b.Bytes())
	default:
		failf(w, "unknown page %v", req.Page)
	}
}

func serveGzipFile(w http.ResponseWriter, r *http.Request, path string, src io.Reader) {
	if acceptsGzip(r) {
		w.Header().Set("Content-Encoding", "gzip")
		io.Copy(w, src) // nothing to do for errors
	} else {
		gzr, err := gzip.NewReader(src)
		if err != nil {
			log.Printf("decompressing %q: %s", path, err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
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

func build(w http.ResponseWriter, r *http.Request, req request) bool {
	start := time.Now()

	err := ensureSDK(req.Goversion)
	if err != nil {
		failf(w, "missing toolchain %q: %v", req.Goversion, err)
		return false
	}

	gobin := fmt.Sprintf("%s/sdk/%s/bin/go", workdir, req.Goversion)
	_, err = os.Stat(gobin)
	if err != nil {
		failf(w, "unknown toolchain %q: %v", req.Goversion, err)
		return false
	}

	dir, err := ioutil.TempDir("", "gobuild")
	if err != nil {
		failf(w, "tempdir for build: %v", err)
		return false
	}
	defer os.RemoveAll(dir)

	homedir := workdir + "/home"
	os.Mkdir(homedir, 0777)

	cmd := exec.CommandContext(r.Context(), gobin, "get", req.Mod+"@"+req.Version)
	cmd.Dir = dir
	cmd.Env = []string{
		"GOPROXY=https://proxy.golang.org",
		"GO111MODULE=on",
		"HOME=" + homedir,
	}
	getOutput, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("output: %s", string(getOutput))
		failf(w, "fetching module: %v", err)
		return false
	}

	var name string
	if req.Dir != "" {
		t := strings.Split(req.Dir[:len(req.Dir)-1], "/")
		name = t[len(t)-1]
	} else {
		t := strings.Split(req.Mod, "/")
		name = t[len(t)-1]
	}
	lname := dir + "/bin/" + name
	os.Mkdir(filepath.Dir(lname), 0777)
	cmd = exec.CommandContext(r.Context(), gobin, "build", "-o", lname, "-x", "-trimpath", "-ldflags", "-buildid=00000000000000000000/00000000000000000000/00000000000000000000/00000000000000000000")
	cmd.Env = []string{
		"CGO_ENABLED=0",
		"GOOS=" + req.Goos,
		"GOARCH=" + req.Goarch,
		"HOME=" + homedir,
	}
	cmd.Dir = homedir + "/go/pkg/mod/" + req.Mod + "@" + req.Version
	if req.Dir != "" {
		cmd.Dir += "/" + req.Dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		failf(w, "running build: %v", err)
		return false
	}

	err = saveFiles(req, output, lname, start, cmd.ProcessState.SystemTime(), cmd.ProcessState.UserTime())
	if err != nil {
		failf(w, "writing resulting files: %v", err)
		return false
	}
	return true
}

func saveFiles(req request, logOutput []byte, lname string, start time.Time, systemTime, userTime time.Duration) error {
	of, err := os.Open(lname)
	if err != nil {
		return err
	}
	defer of.Close()

	fi, err := of.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()

	h := sha256.New()
	_, err = io.Copy(h, of)
	if err != nil {
		return err
	}
	sha256 := fmt.Sprintf("%x", h.Sum(nil))
	sum := sha256[:40]
	_, err = of.Seek(0, io.SeekStart)
	if err != nil {
		return err
	}

	dir := "data/" + req.destdir()

	success := false
	defer func() {
		if !success {
			os.RemoveAll(dir)
		}
	}()
	os.MkdirAll(dir, 0777)

	err = ioutil.WriteFile(dir+"/sha256", []byte(sha256), 0666)
	if err != nil {
		return err
	}

	lf, err := os.Create(dir + "/log.gz")
	if err != nil {
		return err
	}
	defer func() {
		if lf != nil {
			lf.Close()
		}
	}()
	lfgz := gzip.NewWriter(lf)
	_, err = lfgz.Write(logOutput)
	if err != nil {
		return err
	}
	err = lfgz.Close()
	if err != nil {
		return err
	}
	err = lf.Close()
	lf = nil
	if err != nil {
		return err
	}

	nf, err := os.Create(dir + "/" + sum + ".gz")
	if err != nil {
		return err
	}
	defer func() {
		if nf != nil {
			nf.Close()
		}
	}()
	nfgz := gzip.NewWriter(nf)
	_, err = io.Copy(nfgz, of)
	if err != nil {
		return err
	}
	err = nfgz.Close()
	if err != nil {
		return err
	}
	err = nf.Close()
	nf = nil
	if err != nil {
		return err
	}

	bf, err := os.OpenFile("data/builds.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() {
		if bf != nil {
			bf.Close()
		}
	}()
	_, err = fmt.Fprintf(bf, "v1 %s %d %d %d %d %d %s %s %s %s %s %s\n", sha256, size, start.UnixNano()/int64(time.Millisecond), time.Since(start)/time.Millisecond, systemTime/time.Millisecond, userTime/time.Millisecond, req.Goos, req.Goarch, req.Goversion, req.Mod, req.Version, req.Dir)
	if err != nil {
		return err
	}
	err = bf.Close()
	if err != nil {
		return err
	}

	success = true
	return nil
}
