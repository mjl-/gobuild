package main

import (
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// serveIndex serves the HTML page for a build/result, that has either failed or is
// pending under /b/, or has succeeded under /r/.
func serveIndex(w http.ResponseWriter, r *http.Request, req request, result *buildJSON) {
	urlPath := req.buildRequest().urlPath()
	lpath := filepath.Join(config.DataDir, "result", req.storeDir())

	type versionLink struct {
		Version string
		URLPath string
		Success bool
		Active  bool
	}
	type response struct {
		Err          error
		VersionLinks []versionLink
	}
	c := make(chan response, 1)
	go func() {
		t0 := time.Now()
		defer func() {
			metricGoproxyListDuration.Observe(time.Since(t0).Seconds())
		}()

		modPath, err := module.EscapePath(req.Mod)
		if err != nil {
			c <- response{fmt.Errorf("bad module path: %v", err), nil}
			return
		}
		u := fmt.Sprintf("%s%s/@v/list", config.GoProxy, modPath)
		mreq, err := http.NewRequestWithContext(r.Context(), "GET", u, nil)
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
		if resp.StatusCode != 200 {
			metricGoproxyListErrors.WithLabelValues(fmt.Sprintf("%d", resp.StatusCode)).Inc()
			c <- response{fmt.Errorf("%w: http responss from goproxy: %v", errRemote, resp.Status), nil}
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
				vreq := req.buildRequest()
				vreq.Version = s
				success := fileExists(filepath.Join(config.DataDir, "result", vreq.storeDir(), "@index"))
				p := vreq.urlPath()
				link := versionLink{s, p, success, p == urlPath}
				l = append(l, link)
			}
		}
		sort.Slice(l, func(i, j int) bool {
			return semver.Compare(l[i].Version, l[j].Version) > 0
		})
		c <- response{nil, l}
	}()

	// Non-emptiness means we'll serve the error page instead of doing a SSE request for events.
	var output string

	if result == nil {
		buf, err := readGzipFile(filepath.Join(lpath, "log.gz"))
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
		URLPath   string
		Success   bool
		Supported bool
		Active    bool
	}
	goversionLinks := []goversionLink{}
	supported, remaining := installedSDK()
	for _, goversion := range supported {
		gvreq := req.buildRequest()
		gvreq.Goversion = goversion
		success := fileExists(filepath.Join(config.DataDir, "result", gvreq.storeDir(), "@index"))
		p := gvreq.urlPath()
		goversionLinks = append(goversionLinks, goversionLink{goversion, p, success, true, p == urlPath})
	}
	for _, goversion := range remaining {
		gvreq := req.buildRequest()
		gvreq.Goversion = goversion
		success := fileExists(filepath.Join(config.DataDir, "result", gvreq.storeDir(), "@index"))
		p := gvreq.urlPath()
		goversionLinks = append(goversionLinks, goversionLink{goversion, p, success, false, p == urlPath})
	}

	type targetLink struct {
		Goos    string
		Goarch  string
		URLPath string
		Success bool
		Active  bool
	}
	targetLinks := []targetLink{}
	for _, target := range targets.get() {
		treq := req.buildRequest()
		treq.Goos = target.Goos
		treq.Goarch = target.Goarch
		success := fileExists(filepath.Join(config.DataDir, "result", treq.storeDir(), "@index"))
		p := treq.urlPath()
		targetLinks = append(targetLinks, targetLink{target.Goos, target.Goarch, p, success, p == urlPath})
	}

	pkgGoDevURL := fmt.Sprintf("https://pkg.go.dev/%s@%s/%s", req.Mod, req.Version, req.Dir)
	pkgGoDevURL = pkgGoDevURL[:len(pkgGoDevURL)-1] + "?tab=doc"

	resp := <-c

	success := result != nil

	var bsum string
	if success {
		bsum = "0" + base64.RawURLEncoding.EncodeToString(result.SHA256[:20])
	} else {
		result = &buildJSON{} // for easier code below, we always dereference
	}

	args := map[string]interface{}{
		"Success":          success,
		"Req":              req,
		"GoversionLinks":   goversionLinks,
		"TargetLinks":      targetLinks,
		"Mod":              resp,
		"GoProxy":          config.GoProxy,
		"DownloadFilename": req.downloadFilename(),

		// Whether we will do SSE request for updates.
		"InProgress": !success && output == "",

		// Non-empty on failure.
		"Output": output,

		// Below only meaningful when "success".
		"SHA256":          fmt.Sprintf("%x", result.SHA256),
		"Sum":             bsum,
		"Filesize":        fmt.Sprintf("%.1f MB", float64(result.Filesize)/(1024*1024)),
		"FilesizeGz":      fmt.Sprintf("%.1f MB", float64(result.FilesizeGz)/(1024*1024)),
		"Start":           result.Start.Format("2006-01-02 15:04:05"),
		"BuildWallTimeMS": fmt.Sprintf("%d", result.BuildWallTime/time.Millisecond),
		"SystemTimeMS":    fmt.Sprintf("%d", result.SystemTime/time.Millisecond),
		"UserTimeMS":      fmt.Sprintf("%d", result.UserTime/time.Millisecond),
		"PkgGoDevURL":     pkgGoDevURL,
		"GobuildVersion":  gobuildVersion,
	}

	if req.isBuild() {
		w.Header().Set("Cache-Control", "no-store")
	}

	err := buildTemplate.Execute(w, args)
	if err != nil {
		failf(w, "%w: executing template: %v", errServer, err)
	}
}
