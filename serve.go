package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

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
		return r.DownloadSum
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

// we'll get paths like linux-amd64-go1.13/example.com/user/repo/@version/cmd/dir/{log,sha256,dl,<sum>}
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
	// - example.com/user/repo/@version/cmd/dir
	// - example.com/user/repo/@version
	t = strings.SplitN(s, "@", 2)
	if len(t) != 2 {
		return
	}
	r.Mod = t[0]
	if !strings.HasSuffix(r.Mod, "/") {
		return
	}
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

func serveBuilds(w http.ResponseWriter, r *http.Request) {
	req, ok := parsePath(r.URL.Path[3:])
	if !ok {
		http.NotFound(w, r)
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
		var modVersion struct {
			Version string
			Time    time.Time
		}
		u := fmt.Sprintf("%s%s@latest", config.GoProxy, req.Mod)
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		mreq, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			failf(w, "preparing goproxy http request: %v", err)
			return
		}
		resp, err := http.DefaultClient.Do(mreq)
		if err != nil {
			failf(w, "resolving latest at goproxy: %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			failf(w, "error response from goproxy resolving latest, status %s", resp.Status)
			return
		}
		err = json.NewDecoder(resp.Body).Decode(&modVersion)
		if err != nil {
			failf(w, "parsing json returned by goproxy for latest version: %v", err)
			return
		}
		if modVersion.Version == "" {
			failf(w, "empty version for latest from goproxy")
			return
		}
		p := fmt.Sprintf("/x/%s-%s-%s/%s@%s/%s%s", req.Goos, req.Goarch, req.Goversion, req.Mod, modVersion.Version, req.Dir, req.pagePart())
		http.Redirect(w, r, p, http.StatusFound)
		return
	}

	destdir := req.destdir()
	lpath := path.Join(config.DataDir, destdir)
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
		type versionLink struct {
			Version   string
			Path      string
			Available bool
			Active    bool
		}
		type response struct {
			Err          error
			VersionLinks []versionLink
		}
		c := make(chan response, 1)
		go func() {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			u := fmt.Sprintf("%s%s@v/list", config.GoProxy, req.Mod)
			mreq, err := http.NewRequestWithContext(ctx, "GET", u, nil)
			if err != nil {
				c <- response{fmt.Errorf("preparing new http request: %v", err), nil}
				return
			}
			resp, err := http.DefaultClient.Do(mreq)
			if err != nil {
				c <- response{fmt.Errorf("http request: %v", err), nil}
				return
			}
			defer resp.Body.Close()
			if err != nil {
				c <- response{fmt.Errorf("response from goproxy: %v", err), nil}
				return
			}
			buf, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				c <- response{fmt.Errorf("reading versions from goproxy: %v", err), nil}
				return
			}
			l := []versionLink{}
			for _, s := range strings.Split(string(buf), "\n") {
				if s != "" {
					p := fmt.Sprintf("%s-%s-%s/%s@%s/%s", req.Goos, req.Goarch, req.Goversion, req.Mod, s, req.Dir)
					link := versionLink{s, p, false, p == destdir}
					l = append(l, link)
				}
			}
			// todo: do better job of sorting versions; proxy.golang.org doesn't seem to sort them.
			sort.Slice(l, func(i, j int) bool {
				return l[j].Version < l[i].Version
			})
			c <- response{nil, l}
		}()

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
			Supported bool
			Active    bool
		}
		goversionLinks := []goversionLink{}
		supported, remaining := installedSDK()
		for _, goversion := range supported {
			p := fmt.Sprintf("%s-%s-%s/%s@%s/%s", req.Goos, req.Goarch, goversion, req.Mod, req.Version, req.Dir)
			goversionLinks = append(goversionLinks, goversionLink{goversion, p, false, true, p == destdir})
		}
		for _, goversion := range remaining {
			p := fmt.Sprintf("%s-%s-%s/%s@%s/%s", req.Goos, req.Goarch, goversion, req.Mod, req.Version, req.Dir)
			goversionLinks = append(goversionLinks, goversionLink{goversion, p, false, false, p == destdir})
		}

		type targetLink struct {
			Goos      string
			Goarch    string
			Path      string
			Available bool
			Active    bool
		}
		targetLinks := []targetLink{}
		for _, target := range targets {
			p := fmt.Sprintf("%s-%s-%s/%s@%s/%s", target.Goos, target.Goarch, req.Goversion, req.Mod, req.Version, req.Dir)
			targetLinks = append(targetLinks, targetLink{target.Goos, target.Goarch, p, false, p == destdir})
		}

		resp := <-c

		availableBuilds.Lock()
		for i, link := range goversionLinks {
			_, ok := availableBuilds.index[link.Path]
			goversionLinks[i].Available = ok
		}
		for i, link := range targetLinks {
			_, ok := availableBuilds.index[link.Path]
			targetLinks[i].Available = ok
		}
		for i, link := range resp.VersionLinks {
			_, ok := availableBuilds.index[link.Path]
			resp.VersionLinks[i].Available = ok
		}
		availableBuilds.Unlock()

		args := map[string]interface{}{
			"Req":            req,
			"Sum":            sum,
			"GoversionLinks": goversionLinks,
			"TargetLinks":    targetLinks,
			"Mod":            resp,
		}
		err = buildTemplate.Execute(w, args)
		if err != nil {
			failf(w, "executing html template: %v", err)
			return
		}
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