package main

import (
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
)

type page int

const (
	pageIndex page = iota
	pageLog
	pageSha256
	pageDownloadRedirect
	pageDownload
	pageDownloadGz
	pageBuildJSON
)

var pageNames = []string{"index", "log", "sha256", "dlredir", "download", "downloadgz", "buildjson"}

func (p page) String() string {
	return pageNames[p]
}

type request struct {
	Mod         string // Ends with slash, eg github.com/mjl-/gobuild/
	Version     string // Either explicit version like "v0.1.2", or "latest".
	Dir         string // Either empty, or ending with a slash.
	Goos        string
	Goarch      string
	Goversion   string // Eg "go1.14.1" or "latest".
	Page        page
	DownloadSum string
}

func (r request) destdir() string {
	return fmt.Sprintf("%s-%s-%s/%s@%s/%s", r.Goos, r.Goarch, r.Goversion, r.Mod, r.Version, r.Dir)
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

// name of file the http user-agent (browser) will save the file as.
func (r request) downloadFilename() string {
	ext := ""
	if r.Goos == "windows" {
		ext = ".exe"
	}
	return fmt.Sprintf("%s-%s-%s%s", r.filename(), r.Version, r.Goversion, ext)
}

// we'll get paths like linux-amd64-go1.13/example.com/user/repo/@version/cmd/dir/{log,sha256,dl,<name>,<name>.gz,build.json}
func parsePath(s string) (r request, hint string, ok bool) {
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

	var downloadName string
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
	case strings.HasSuffix(s, "/build.json"):
		r.Page = pageBuildJSON
		s = s[:len(s)-len("/build.json")]
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

	// We are left parsing eg:
	// - example.com/user/repo/@version/cmd/dir
	// - example.com/user/repo/@version
	t = strings.SplitN(s, "@", 2)
	if len(t) != 2 {
		hint = `Perhaps a missing "@" version in URL?`
		return
	}
	r.Mod = t[0]
	if !strings.HasSuffix(r.Mod, "/") {
		hint = "Perhaps a missing / at end of module?"
		return
	}
	s = t[1]
	t = strings.SplitN(s, "/", 2)
	r.Version = t[0]
	if len(t) == 2 {
		r.Dir = t[1] + "/"
	}

	hint = "Perhaps a missing / at end of package?"
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
	msg := fmt.Sprintf(format, args...)
	log.Println(msg)
	http.Error(w, "500 - "+msg, http.StatusInternalServerError)
}

func ufailf(w http.ResponseWriter, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	http.Error(w, "400 - "+msg, http.StatusBadRequest)
}

func serveBuilds(w http.ResponseWriter, r *http.Request) {
	req, hint, ok := parsePath(r.URL.Path[3:])
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
		p := fmt.Sprintf("/x/%s-%s-%s/%s@%s/%s%s", req.Goos, req.Goarch, goversion, req.Mod, req.Version, req.Dir, req.pagePart())
		http.Redirect(w, r, p, http.StatusFound)
		return
	}

	// Resolve "latest" module version with a redirect.
	if req.Version == "latest" {
		info, err := resolveModuleLatest(r.Context(), req.Mod)
		if err != nil {
			failf(w, "resolving latest for module: %v", err)
			return
		}

		p := fmt.Sprintf("/x/%s-%s-%s/%s@%s/%s%s", req.Goos, req.Goarch, req.Goversion, req.Mod, info.Version, req.Dir, req.pagePart())
		http.Redirect(w, r, p, http.StatusFound)
		return
	}

	destdir := req.destdir()
	lpath := path.Join(config.DataDir, destdir)

	// Presence of log.gz file indicates if a build was done before, whether successful or failed.
	_, err := os.Stat(lpath + "/log.gz")
	if err != nil {
		if !os.IsNotExist(err) {
			failf(w, "stat path: %v", err)
			return
		}

		metricBuilds.WithLabelValues(req.Goos, req.Goarch, req.Goversion).Inc()
		ok, tmpFail := build(w, r, req)
		if !ok && tmpFail {
			return
		}
		p := fmt.Sprintf("%s-%s-%s/%s@%s/%s", req.Goos, req.Goarch, req.Goversion, req.Mod, req.Version, req.Dir)
		if !ok {
			metricBuildErrors.WithLabelValues(req.Goos, req.Goarch, req.Goversion).Inc()
		} else {
			recentBuilds.Lock()
			recentBuilds.paths = append(recentBuilds.paths, p)
			if len(recentBuilds.paths) > 10 {
				recentBuilds.paths = recentBuilds.paths[len(recentBuilds.paths)-10:]
			}
			recentBuilds.Unlock()
		}
		availableBuilds.Lock()
		availableBuilds.index[p] = ok
		availableBuilds.Unlock()
	}

	bf, err := os.Open(lpath + "/build.json")
	if err == nil {
		defer bf.Close()
	}
	var buildResult buildJSON
	if err == nil {
		err = json.NewDecoder(bf).Decode(&buildResult)
	}
	if err != nil && !os.IsNotExist(err) {
		failf(w, "reading build.json: %v", err)
		return
	}
	if err == nil {
		// Redirect to the permanent URLs that include the hash.
		bsum := base64.RawURLEncoding.EncodeToString(buildResult.SHA256[:20])
		p := fmt.Sprintf("/z/%s/%s-%s-%s/%s@%s/%s%s", bsum, req.Goos, req.Goarch, req.Goversion, req.Mod, req.Version, req.Dir, req.pagePart())
		http.Redirect(w, r, p, http.StatusFound)
		return
	}

	// Show failed build to user, for the pages where that works.
	switch req.Page {
	case pageLog:
		serveLog(w, r, lpath+"/log.gz")
	case pageIndex:
		serveBuildIndex(w, r, req, nil)
	default:
		http.Error(w, "400 - bad request, build failed", http.StatusBadRequest)
	}
}

func serveLog(w http.ResponseWriter, r *http.Request, p string) {
	f, err := os.Open(p)
	if err != nil {
		failf(w, "open log.gz: %v", err)
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
