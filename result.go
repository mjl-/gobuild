package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

func serveResults(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "405 - Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	t := strings.SplitN(r.URL.Path[3:], "/", 2)
	if len(t) != 2 || t[1] == "" {
		http.NotFound(w, r)
		return
	}
	bsum, urlPath := t[0], t[1]
	sum, err := base64.RawURLEncoding.DecodeString(bsum)
	if err != nil || len(sum) != 20 {
		http.NotFound(w, r)
		return
	}

	req, hint, ok := parsePath(urlPath)
	if !ok {
		if hint != "" {
			http.Error(w, fmt.Sprintf("404 - File Not Found\n\n%s\n", hint), http.StatusNotFound)
		} else {
			http.NotFound(w, r)
		}
		return
	}

	destdir := req.destdir()
	lpath := path.Join(config.DataDir, destdir)

	bf, err := os.Open(lpath + "/build.json")
	if err == nil {
		defer bf.Close()
	}
	var buildResult buildJSON
	if err == nil {
		err = json.NewDecoder(bf).Decode(&buildResult)
	}
	if err != nil && os.IsNotExist(err) || err == nil && bytes.Equal(buildResult.SHA256, []byte{'x'}) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		failf(w, "%w: reading build.json: %v", errServer, err)
		return
	}
	if base64.RawURLEncoding.EncodeToString(buildResult.SHA256[:20]) != bsum {
		http.NotFound(w, r)
		return
	}

	switch req.Page {
	case pageLog:
		serveLog(w, r, lpath+"/log.gz")
	case pageSha256:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "%x\n", buildResult.SHA256) // nothing to do for errors
	case pageDownloadRedirect:
		p := "/z/" + bsum + "/" + req.destdir() + req.downloadFilename()
		http.Redirect(w, r, p, http.StatusFound)
	case pageDownload:
		p := path.Join(lpath, req.downloadFilename()+".gz")
		f, err := os.Open(p)
		if err != nil {
			failf(w, "%w: open binary: %v", errServer, err)
			return
		}
		defer f.Close()
		serveGzipFile(w, r, p, f)
	case pageDownloadGz:
		p := path.Join(lpath, req.downloadFilename()+".gz")
		http.ServeFile(w, r, p)
	case pageBuildJSON:
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(buildResult) // nothing to do for errors
	case pageIndex:
		serveBuildIndex(w, r, req, &buildResult)
	default:
		failf(w, "%w: unknown page %v", errServer, req.Page)
	}
}

// serveBuildIndex serves the HTML page for a build that has either failed or in pending (under /x/), or has succeeded (under /z/).
func serveBuildIndex(w http.ResponseWriter, r *http.Request, req request, result *buildJSON) {
	destdir := req.destdir()
	lpath := path.Join(config.DataDir, destdir)

	type versionLink struct {
		Version   string
		Path      string
		Available bool
		Success   bool
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
			c <- response{fmt.Errorf("%w: preparing new http request: %v", errServer, err), nil}
			return
		}
		resp, err := http.DefaultClient.Do(mreq)
		if err != nil {
			c <- response{fmt.Errorf("%w: http request: %v", errServer, err), nil}
			return
		}
		defer resp.Body.Close()
		if err != nil {
			c <- response{fmt.Errorf("%w: response from goproxy: %v", errRemote, err), nil}
			return
		}
		buf, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			c <- response{fmt.Errorf("%w: reading versions from goproxy: %v", errRemote, err), nil}
			return
		}
		l := []versionLink{}
		for _, s := range strings.Split(string(buf), "\n") {
			if s != "" {
				p := fmt.Sprintf("%s-%s-%s/%s@%s/%s", req.Goos, req.Goarch, req.Goversion, req.Mod, s, req.Dir)
				link := versionLink{s, p, false, false, p == destdir}
				l = append(l, link)
			}
		}
		// todo: do better job of sorting versions; proxy.golang.org doesn't seem to sort them.
		sort.Slice(l, func(i, j int) bool {
			return l[j].Version < l[i].Version
		})
		c <- response{nil, l}
	}()

	// Non-emptiness means we'll serve the error page instead of doing a SSE request for events.
	var output string

	if result == nil {
		buf, err := readGzipFile(lpath + "/log.gz")
		if err != nil {
			if !os.IsNotExist(err) {
				failf(w, "%w: reading log.gz: %v", errServer, err)
				return
			}
		} else {
			output = string(buf)
		}
	}

	type goversionLink struct {
		Goversion string
		Path      string
		Available bool
		Success   bool
		Supported bool
		Active    bool
	}
	goversionLinks := []goversionLink{}
	supported, remaining := installedSDK()
	for _, goversion := range supported {
		p := fmt.Sprintf("%s-%s-%s/%s@%s/%s", req.Goos, req.Goarch, goversion, req.Mod, req.Version, req.Dir)
		goversionLinks = append(goversionLinks, goversionLink{goversion, p, false, false, true, p == destdir})
	}
	for _, goversion := range remaining {
		p := fmt.Sprintf("%s-%s-%s/%s@%s/%s", req.Goos, req.Goarch, goversion, req.Mod, req.Version, req.Dir)
		goversionLinks = append(goversionLinks, goversionLink{goversion, p, false, false, false, p == destdir})
	}

	type targetLink struct {
		Goos      string
		Goarch    string
		Path      string
		Available bool
		Success   bool
		Active    bool
	}
	targetLinks := []targetLink{}
	for _, target := range targets {
		p := fmt.Sprintf("%s-%s-%s/%s@%s/%s", target.Goos, target.Goarch, req.Goversion, req.Mod, req.Version, req.Dir)
		targetLinks = append(targetLinks, targetLink{target.Goos, target.Goarch, p, false, false, p == destdir})
	}

	pkgGoDevURL := fmt.Sprintf("https://pkg.go.dev/%s@%s/%s", req.Mod, req.Version, req.Dir)
	pkgGoDevURL = pkgGoDevURL[:len(pkgGoDevURL)-1] + "?tab=doc"

	resp := <-c

	availableBuilds.Lock()
	for i, link := range goversionLinks {
		goversionLinks[i].Success, goversionLinks[i].Available = availableBuilds.index[link.Path]
	}
	for i, link := range targetLinks {
		targetLinks[i].Success, targetLinks[i].Available = availableBuilds.index[link.Path]
	}
	for i, link := range resp.VersionLinks {
		resp.VersionLinks[i].Success, resp.VersionLinks[i].Available = availableBuilds.index[link.Path]
	}
	availableBuilds.Unlock()

	success := result != nil

	var bsum string
	if success {
		bsum = base64.RawURLEncoding.EncodeToString(result.SHA256[:20])
	} else {
		result = &buildJSON{} // for easier code below, we always dereference
	}

	args := map[string]interface{}{
		"Success":        success,
		"Req":            req,
		"GoversionLinks": goversionLinks,
		"TargetLinks":    targetLinks,
		"Mod":            resp,

		// Whether we will do SSE request for updates.
		"InProgress": !success && output == "",

		// Non-empty on failure.
		"Output": output,

		// Below only meaningful when "success".
		"SHA256":           result.SHA256,
		"Sum":              bsum,
		"DownloadFilename": req.downloadFilename(),
		"Filesize":         fmt.Sprintf("%.1f MB", float64(result.Filesize)/(1024*1024)),
		"FilesizeGz":       fmt.Sprintf("%.1f MB", float64(result.FilesizeGz)/(1024*1024)),
		"Start":            result.Start.Format("2006-01-02 15:04:05"),
		"BuildWallTimeMS":  fmt.Sprintf("%d", result.BuildWallTime/time.Millisecond),
		"SystemTimeMS":     fmt.Sprintf("%d", result.SystemTime/time.Millisecond),
		"UserTimeMS":       fmt.Sprintf("%d", result.UserTime/time.Millisecond),
		"PkgGoDevURL":      pkgGoDevURL,
	}
	err := buildTemplate.Execute(w, args)
	if err != nil {
		failf(w, "%w: executing template: %v", errServer, err)
	}
}

func readGzipFile(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fgz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	return ioutil.ReadAll(fgz)
}
