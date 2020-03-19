// Gobuild serves reproducibly built binaries from sources via go module proxy.
//
// Serves URLs like:
//
// 	http://localhost:8000/
// 	http://localhost:8000/x/github.com/mjl-/sherpa@v0.6.0/cmd/sherpaclient/linux-amd64-go1.13
// 	http://localhost:8000/x/github.com/mjl-/sherpa@v0.6.0/cmd/sherpaclient/linux-amd64-go1.13-$sha256
// 	http://localhost:8000/x/github.com/mjl-/sherpa@v0.6.0/cmd/sherpaclient/linux-amd64-go1.13.{log,sha256}
package main

import (
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"

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

	tmpl, err := template.New("").Parse(`<h1>gobuild - reproducible binaries with the go module proxy</h1>
		<p>The Go team runs the <a href="https://proxy.golang.org/">Go module proxy</a>. This ensures code stays available, and you are likely to get the same code each time you fetch it. This helps you make reproducible builds. But you still have to build it yourself.</p>
		<p>Gobuild actually compiles Go code available through the Go module proxy, and returns the binary.</p>

		<h2>URLs</h2>
		<p>Composition of URLs:</p>
		<blockquote style="color:#666">https://gobuild.ueber.net/x/<span style="color:#111">&lt;goos&gt;</span>-<span style="color:#111">&lt;goarch&gt;</span>-<span style="color:#111">&lt;goversion&gt;</span>/<span style="color:#111">&lt;module&gt;</span>@<span style="color:#111">&lt;version&gt;</span>/<span style="color:#111">&lt;package&gt;</span>/</blockquote>
		<p>Example:</p>
		<blockquote><a href="/x/linux-amd64-go1.14/github.com/mjl-/sherpa@v0.6.0/cmd/sherpaclient/">https://gobuild.ueber.net/x/linux-amd64-go1.14/github.com/mjl-/sherpa@v0.6.0/cmd/sherpaclient/</a></blockquote>
		<p>Opening this URL will either start a build, or show the results of an earlier build. The page shows links to download the binary, view the build output log file, the sha256 sum of the binary. You'll also see cross references to builds with different goversion, goos, goarch, and different versions of the module. You need not and cannot refresh a build, because they are reproducible.</p>

		<h2>Recent builds</h2>
		<ul>
{{ range . }}			<li><a href="/x/{{ . }}">{{ . }}</a></li>{{ end }}
		</ul>

		<h2>Details</h2>
		<p>Builds are created with CGO_ENABLED=0, -trimpath flag, and an zero buildid.</p>
		<p>Only "go build" is run. No tests, no generate, no makefiles, etc.</p>
		<p>Code is available at <a href="https://github.com/mjl-/gobuild">github.com/mjl-/gobuild</a>, under MIT-license.</p>
`)
	if err != nil {
		failf(w, "parsing template: %v", err)
		return
	}
	b := &bytes.Buffer{}

	recentBuilds.Lock()
	recents := recentBuilds.paths
	recentBuilds.Unlock()

	err = tmpl.Execute(b, recents)
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
	return fmt.Sprintf("%s-%s-%s", name, r.Version, r.Goversion)
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
	}

	switch req.Page {
	case pageLog:
		f, err := os.Open(lpath + "/log")
		if err != nil {
			failf(w, "open log: %v", err)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.Copy(w, f)
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
		f, err := os.Open(lpath + "/" + req.DownloadSum)
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
		io.Copy(w, f)
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

		b := &bytes.Buffer{}
		tmpl, err := template.New("").Parse(`<p><a href="/">&lt; Home</a></p>
<h1>{{ .Req.Mod }}@{{ .Req.Version }}/{{ .Req.Dir }}</h1>
<h2>{{ .Req.Goos }}/{{ .Req.Goarch }} {{ .Req.Goversion }}</h2>
<ul>
	<li><a href="{{ .Sum }}">Download</a></li>
	<li><a href="log">Log</a></li>
	<li><a href="sha256">Sha256</a> ({{ .Sum }})</li>
</ul>

{{ $req := .Req }}
<div style="width: 32%; display: inline-block; vertical-align: top">
	<h2>Go versions</h2>
{{ range .Goversions }}	<div><a href="/x/{{ $req.Goos }}-{{ $req.Goarch }}-{{ . }}/{{ $req.Mod }}@{{ $req.Version }}/ {{ $req.Dir }}">{{ . }}</a></div>{{ end }}
</div>

<div style="width: 32%; display: inline-block; vertical-align: top">
	<h2>Module versions</h2>
	<div>TODO: fetch from module proxy/mirror/index</div>
</div>

<div style="width: 32%; display: inline-block; vertical-align: top">
	<h2>Targets</h2>
{{ range .Targets }}	<div><a href="/x/{{ .Goos }}-{{ .Goarch }}-{{ $req.Goversion }}/{{ $req.Mod }}@{{ $req.Version }}/{{ $req.Dir }}">{{ .Goos }}/{{ .Goarch }}</a></div>{{ end }}
</div>
`)
		if err != nil {
			failf(w, "parsing html template: %v", err)
			return
		}
		args := map[string]interface{}{
			"Req":        req,
			"Sum":        sum,
			"Goversions": installedSDK(),
			"Targets":    targets,
		}
		err = tmpl.Execute(b, args)
		if err != nil {
			failf(w, "executing html template: %v", err)
			return
		}
		writeHTML(w, b.Bytes())
	default:
		failf(w, "unknown page %v", req.Page)
	}
}

func build(w http.ResponseWriter, r *http.Request, req request) bool {
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

	err = saveFiles(req, output, lname)
	if err != nil {
		failf(w, "writing resulting files: %v", err)
		return false
	}
	return true
}

func saveFiles(req request, logOutput []byte, lname string) error {
	of, err := os.Open(lname)
	if err != nil {
		return err
	}
	defer of.Close()

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

	err = ioutil.WriteFile(dir+"/log", logOutput, 0666)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(dir+"/sha256", []byte(sha256), 0666)
	if err != nil {
		return err
	}

	nf, err := os.Create(dir + "/" + sum)
	if err != nil {
		return err
	}
	defer func() {
		if nf != nil {
			nf.Close()
		}
	}()
	_, err = io.Copy(nf, of)
	if err != nil {
		return err
	}
	err = nf.Close()
	if err != nil {
		return err
	}
	nf = nil

	bf, err := os.OpenFile("data/builds.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() {
		if bf != nil {
			bf.Close()
		}
	}()
	_, err = fmt.Fprintf(bf, "%s %s %s %s %s %s\n", req.Goos, req.Goarch, req.Goversion, req.Mod, req.Version, req.Dir)
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
