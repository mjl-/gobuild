package main

import (
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
)

var errRemote = errors.New("remote")
var errServer = errors.New("server error")

type page int

const (
	pageIndex page = iota
	pageLog
	pageSha256
	pageDownloadRedirect
	pageDownload
	pageDownloadGz
	pageBuildJSON
	pageEvents
)

var pageNames = []string{"index", "log", "sha256", "dlredir", "download", "downloadgz", "buildjson", "events"}

func (p page) String() string {
	return pageNames[p]
}

type request struct {
	Mod       string // Eg github.com/mjl-/gobuild
	Version   string // Either explicit version like "v0.1.2", or "latest".
	Dir       string // Either empty, or ending with a slash.
	Goos      string
	Goarch    string
	Goversion string // Eg "go1.14.1" or "latest".
	Page      page
	Sum       string // If set, indicates a request on /r/.
}

// buildRequest returns a request that points to a /b/ URL, leaving page intact.
func (r request) buildRequest() request {
	r.Sum = ""
	return r
}

func (r request) buildIndexRequest() request {
	r.Page = pageIndex
	r.Sum = ""
	return r
}

// local directory where results are stored.
func (r request) storeDir() string {
	return fmt.Sprintf("%s-%s-%s/%s@%s/%s", r.Goos, r.Goarch, r.Goversion, r.Mod, r.Version, r.Dir)
}

// Path in URL for this request.
func (r request) urlPath() string {
	var kind string
	if r.Sum == "" {
		kind = "b"
	} else {
		kind = "r"
	}
	s := fmt.Sprintf("/%s/%s@%s/%s%s-%s-%s/", kind, r.Mod, r.Version, r.Dir, r.Goos, r.Goarch, r.Goversion)
	if r.Sum != "" {
		s += r.Sum + "/"
	}
	s += r.pagePart()
	return s
}

func (r request) pagePart() string {
	switch r.Page {
	case pageIndex:
		return ""
	case pageLog:
		return "log"
	case pageSha256:
		return "sha256"
	case pageDownloadRedirect:
		return "dl"
	case pageDownload:
		return r.downloadFilename()
	case pageDownloadGz:
		return r.downloadFilename() + ".gz"
	case pageBuildJSON:
		return "build.json"
	case pageEvents:
		return "events"
	default:
		panic("missing case")
	}
}

func (r request) filename() string {
	if r.Dir != "" {
		return path.Base(r.Dir)
	}
	return path.Base(r.Mod)
}

// Name of file the http user-agent (browser) will save the file as.
func (r request) downloadFilename() string {
	ext := ""
	if r.Goos == "windows" {
		ext = ".exe"
	}
	return fmt.Sprintf("%s-%s-%s%s", r.filename(), r.Version, r.Goversion, ext)
}

// We'll get paths like /[br]/github.com/mjl-/sherpa@v0.6.0/cmd/sherpaclient/linux-amd64-go1.14.1/0rLhZFgnc9hme13PhUpIvNw08LEk/{log,sha256,dl,<name>,<name>.gz,build.json, events}
func parsePath(s string) (r request, hint string, ok bool) {
	withSum := strings.HasPrefix(s, "/r/")
	s = s[len("/X/"):]

	var downloadName string
	switch {
	case strings.HasSuffix(s, "/"):
		r.Page = pageIndex
		s = s[:len(s)-len("/")]
	case strings.HasSuffix(s, "/log"):
		r.Page = pageLog
		s = s[:len(s)-len("/log")]
	case strings.HasSuffix(s, "/sha256"):
		r.Page = pageSha256
		s = s[:len(s)-len("/sha256")]
	case strings.HasSuffix(s, "/dl"):
		r.Page = pageDownloadRedirect
		s = s[:len(s)-len("/dl")]
	case strings.HasSuffix(s, "/build.json"):
		r.Page = pageBuildJSON
		s = s[:len(s)-len("/build.json")]
	case strings.HasSuffix(s, "/events"):
		r.Page = pageEvents
		s = s[:len(s)-len("/events")]
	default:
		t := strings.Split(s, "/")
		downloadName = t[len(t)-1]
		if strings.HasSuffix(downloadName, ".gz") {
			r.Page = pageDownloadGz
		} else {
			r.Page = pageDownload
		}
		s = s[:len(s)-1-len(downloadName)]
		// After parsing module,version,package below, we'll check if the download name is
		// indeed valid for this filename.
	}

	// Remaining: example.com/user/repo@version/cmd/dir/goos-goarch-goversion/sum
	t := strings.SplitN(s, "@", 2)
	if len(t) != 2 {
		hint = "Perhaps missing @version?"
		return
	}
	r.Mod = t[0]
	s = t[1]

	// Remaining: version/cmd/dir/linux-amd64-go1.13/sum
	t = strings.Split(s, "/")
	var sum string
	if withSum {
		if len(t) < 3 {
			hint = "Perhaps missing sum?"
			return
		}
		sum = t[len(t)-1]
		if !strings.HasPrefix(sum, "0") {
			hint = "Perhaps malformed or misversioned sum?"
			return
		}
		xsum, err := base64.RawURLEncoding.DecodeString(sum[1:])
		if err != nil || len(xsum) != 20 {
			hint = "Perhaps malformed sum?"
			return
		}
		r.Sum = sum
		t = t[:len(t)-1]
	}
	if len(t) < 2 {
		hint = "Perhaps missing version or goos-goarch-goversion?"
		return
	}
	tt := strings.Split(t[len(t)-1], "-")
	if len(tt) != 3 {
		hint = "Perhaps bad goos-goarch-goversion?"
		return
	}
	r.Goos = tt[0]
	r.Goarch = tt[1]
	r.Goversion = tt[2]
	t = t[:len(t)-1]
	r.Version = t[0]
	r.Dir = strings.Join(t[1:], "/")
	if r.Dir != "" {
		r.Dir += "/"
	}

	switch r.Page {
	case pageDownload:
		if r.downloadFilename() != downloadName {
			return
		}
	case pageDownloadGz:
		if r.downloadFilename()+".gz" != downloadName {
			return
		}
	}

	ok = true
	return
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

func serveBuild(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "405 - Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	req, hint, ok := parsePath(r.URL.Path)
	if !ok {
		if hint != "" {
			http.Error(w, fmt.Sprintf("404 - File Not Found\n\n%s\n", hint), http.StatusNotFound)
		} else {
			http.NotFound(w, r)
		}
		return
	}

	metricRequestsTotal.WithLabelValues(req.Page.String())

	// Resolve "latest" goversion with a redirect.
	if req.Goversion == "latest" {
		supported, _ := installedSDK()
		if len(supported) == 0 {
			http.Error(w, "no go supported toolchains available", http.StatusServiceUnavailable)
			return
		}
		goversion := supported[0]
		vreq := req
		vreq.Goversion = goversion
		http.Redirect(w, r, vreq.urlPath(), http.StatusFound)
		return
	}

	// Resolve "latest" module version with a redirect.
	if req.Version == "latest" {
		info, err := resolveModuleLatest(r.Context(), req.Mod)
		if err != nil {
			failf(w, "resolving latest for module: %w", err)
			return
		}

		mreq := req
		mreq.Version = info.Version
		http.Redirect(w, r, mreq.urlPath(), http.StatusFound)
		return
	}

	lpath := path.Join(config.DataDir, req.storeDir())

	// If build.json exists, we have a successful build.
	bf, err := os.Open(lpath + "/build.json")
	if err == nil {
		defer bf.Close()
	}
	var buildResult buildJSON
	if err == nil {
		err = json.NewDecoder(bf).Decode(&buildResult)
	}
	if err != nil && !os.IsNotExist(err) {
		failf(w, "%w: reading build.json: %v", errServer, err)
		return
	}
	if err == nil {
		// Redirect to the permanent URLs that include the hash.
		rreq := req
		rreq.Sum = "0" + base64.RawURLEncoding.EncodeToString(buildResult.SHA256[:20])
		http.Redirect(w, r, rreq.urlPath(), http.StatusFound)
		return
	}

	// If log.gz exists, we have a failed build.
	_, err = os.Stat(lpath + "/log.gz")
	if err != nil && !os.IsNotExist(err) {
		failf(w, "%w: stat path: %v", errServer, err)
		return
	}
	if err == nil {
		// Show failed build to user, for the pages where that works.
		switch req.Page {
		case pageLog:
			serveLog(w, r, lpath+"/log.gz")
		case pageIndex:
			serveIndex(w, r, req, nil)
		default:
			http.Error(w, "400 - bad request, build failed", http.StatusBadRequest)
		}
		return
	}

	// No build yet, we need one.

	// We always attempt to set up the build. This checks with the goproxy that the
	// module and package exist, and seems like it has a chance to compile.
	err = prepareBuild(req)
	if err != nil {
		failf(w, "preparing build: %w", err)
		return
	}

	// We serve the index page immediately. It makes an SSE-request to the /events
	// endpoint to register a request for the build and to receive updates.
	if req.Page == pageIndex {
		serveIndex(w, r, req, nil)
		return
	}

	eventc := make(chan buildUpdate, 100)
	registerBuild(req, eventc)

	ctx := r.Context()

	switch req.Page {
	case pageEvents:
		flusher, ok := w.(http.Flusher)
		if !ok {
			log.Println("ResponseWriter not a http.Flusher")
			failf(w, "%w: implementation limitation: cannot stream updates", errServer)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		_, err := w.Write([]byte(": keepalive\n\n"))
		if err != nil {
			return
		}
		flusher.Flush()

	loop:
		for {
			select {
			case <-ctx.Done():
				break loop
			case update := <-eventc:
				_, err = w.Write(update.msg)
				flusher.Flush()
				if update.done || err != nil {
					break loop
				}
			}
		}
		unregisterBuild(req, eventc)
	default:
		for {
			select {
			case <-ctx.Done():
				unregisterBuild(req, eventc)
				return
			case update := <-eventc:
				if !update.done {
					continue
				}
				unregisterBuild(req, eventc)

				if req.Page == pageLog {
					serveLog(w, r, lpath+"/log.gz")
					return
				}

				if update.err != nil {
					failf(w, "build failed: %w", update.err)
					return
				}

				// Redirect to the permanent URLs that include the hash.
				rreq := req
				rreq.Sum = "0" + base64.RawURLEncoding.EncodeToString(update.result.SHA256[:20])
				http.Redirect(w, r, rreq.urlPath(), http.StatusFound)
				return
			}
		}
	}
}

// goBuild performs the actual build. This is called from coordinate.go, not too
// many at a time.
func goBuild(req request) (*buildJSON, error) {
	bu := req.buildIndexRequest().urlPath()

	metricBuilds.WithLabelValues(req.Goos, req.Goarch, req.Goversion).Inc()

	result, err := build(req)

	ok := err == nil && result != nil
	if err == nil || !errors.Is(err, errTempFailure) {
		availableBuilds.Lock()
		availableBuilds.index[bu] = ok
		availableBuilds.Unlock()
	}
	if err != nil {
		metricBuildErrors.WithLabelValues(req.Goos, req.Goarch, req.Goversion).Inc()
		return nil, err
	}

	fp := bu
	if ok {
		rreq := req.buildIndexRequest()
		rreq.Sum = "0" + base64.RawURLEncoding.EncodeToString(result.SHA256[:20])
		fp = rreq.urlPath()
	}
	recentBuilds.Lock()
	recentBuilds.paths = append(recentBuilds.paths, fp)
	if len(recentBuilds.paths) > 10 {
		recentBuilds.paths = recentBuilds.paths[len(recentBuilds.paths)-10:]
	}
	recentBuilds.Unlock()
	return result, nil
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
