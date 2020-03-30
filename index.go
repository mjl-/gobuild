package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// serveIndex serves the HTML page for a build/result, that has either failed or is
// pending under /b/, or has succeeded under /r/.
func serveIndex(w http.ResponseWriter, r *http.Request, req request, result *buildJSON) {
	urlPath := req.buildRequest().urlPath()
	lpath := path.Join(config.DataDir, req.storeDir())

	type versionLink struct {
		Version   string
		URLPath   string
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

		t0 := time.Now()
		defer func() {
			metricGoproxyListDuration.Observe(time.Since(t0).Seconds())
		}()

		u := fmt.Sprintf("%s%s/@v/list", config.GoProxy, goproxyEscape(req.Mod))
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
				p := vreq.urlPath()
				link := versionLink{s, p, false, false, p == urlPath}
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
		URLPath   string
		Available bool
		Success   bool
		Supported bool
		Active    bool
	}
	goversionLinks := []goversionLink{}
	supported, remaining := installedSDK()
	for _, goversion := range supported {
		gvreq := req.buildRequest()
		gvreq.Goversion = goversion
		p := gvreq.urlPath()
		goversionLinks = append(goversionLinks, goversionLink{goversion, p, false, false, true, p == urlPath})
	}
	for _, goversion := range remaining {
		gvreq := req.buildRequest()
		gvreq.Goversion = goversion
		p := gvreq.urlPath()
		goversionLinks = append(goversionLinks, goversionLink{goversion, p, false, false, false, p == urlPath})
	}

	type targetLink struct {
		Goos      string
		Goarch    string
		URLPath   string
		Available bool
		Success   bool
		Active    bool
	}
	targetLinks := []targetLink{}
	for _, target := range targets {
		treq := req.buildRequest()
		treq.Goos = target.Goos
		treq.Goarch = target.Goarch
		p := treq.urlPath()
		targetLinks = append(targetLinks, targetLink{target.Goos, target.Goarch, p, false, false, p == urlPath})
	}

	pkgGoDevURL := fmt.Sprintf("https://pkg.go.dev/%s@%s/%s", req.Mod, req.Version, req.Dir)
	pkgGoDevURL = pkgGoDevURL[:len(pkgGoDevURL)-1] + "?tab=doc"

	resp := <-c

	availableBuilds.Lock()
	for i, link := range goversionLinks {
		goversionLinks[i].Success, goversionLinks[i].Available = availableBuilds.index[link.URLPath]
	}
	for i, link := range targetLinks {
		targetLinks[i].Success, targetLinks[i].Available = availableBuilds.index[link.URLPath]
	}
	for i, link := range resp.VersionLinks {
		resp.VersionLinks[i].Success, resp.VersionLinks[i].Available = availableBuilds.index[link.URLPath]
	}
	availableBuilds.Unlock()

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
	}

	if req.isBuild() {
		w.Header().Set("Cache-Control", "no-store")
	}

	err := buildTemplate.Execute(w, args)
	if err != nil {
		failf(w, "%w: executing template: %v", errServer, err)
	}
}
