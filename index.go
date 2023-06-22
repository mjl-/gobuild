package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
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
// pending or has succeeded.
func serveIndex(w http.ResponseWriter, r *http.Request, bs buildSpec, br *buildResult) {
	xreq := request{bs, "", pageIndex}
	xlink := xreq.link()

	type versionLink struct {
		Version string
		URLPath string
		Success bool
		Active  bool
	}
	type response struct {
		Err           error
		LatestVersion string
		VersionLinks  []versionLink
	}

	// Do a lookup to the goproxy in the background, to list the module versions.
	c := make(chan response, 1)
	go func() {
		t0 := time.Now()
		defer func() {
			metricGoproxyListDuration.Observe(time.Since(t0).Seconds())
		}()

		modPath, err := module.EscapePath(bs.Mod)
		if err != nil {
			c <- response{fmt.Errorf("bad module path: %v", err), "", nil}
			return
		}
		u := fmt.Sprintf("%s%s/@v/list", config.GoProxy, modPath)
		mreq, err := http.NewRequestWithContext(r.Context(), "GET", u, nil)
		if err != nil {
			c <- response{fmt.Errorf("%w: preparing new http request: %v", errServer, err), "", nil}
			return
		}
		mreq.Header.Set("User-Agent", userAgent)
		resp, err := http.DefaultClient.Do(mreq)
		if err != nil {
			c <- response{fmt.Errorf("%w: http request: %v", errServer, err), "", nil}
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			metricGoproxyListErrors.WithLabelValues(fmt.Sprintf("%d", resp.StatusCode)).Inc()
			c <- response{fmt.Errorf("%w: http response from goproxy: %v", errRemote, resp.Status), "", nil}
			return
		}
		buf, err := io.ReadAll(resp.Body)
		if err != nil {
			c <- response{fmt.Errorf("%w: reading versions from goproxy: %v", errRemote, err), "", nil}
			return
		}
		l := []versionLink{}
		for _, s := range strings.Split(string(buf), "\n") {
			if s != "" {
				vbs := bs
				vbs.Version = s
				success := fileExists(filepath.Join(vbs.storeDir(), "recordnumber"))
				p := request{vbs, "", pageIndex}.link()
				link := versionLink{s, p, success, p == xlink}
				l = append(l, link)
			}
		}
		sort.Slice(l, func(i, j int) bool {
			return semver.Compare(l[i].Version, l[j].Version) > 0
		})
		var latestVersion string
		if len(l) > 0 {
			latestVersion = l[0].Version
		}
		c <- response{nil, latestVersion, l}
	}()

	// Non-emptiness means we'll serve the error page instead of doing a SSE request for events.
	var output string
	if br == nil {
		if buf, err := readGzipFile(filepath.Join(bs.storeDir(), "log.gz")); err != nil {
			if !os.IsNotExist(err) {
				failf(w, "%w: reading log.gz: %v", errServer, err)
				return
			}
			// For not-exist, we'll continue below to build.
		} else {
			output = string(buf)
		}
	}

	// Construct links to other goversions, targets.
	type goversionLink struct {
		Goversion string
		URLPath   string
		Success   bool
		Supported bool
		Active    bool
	}
	goversionLinks := []goversionLink{}
	newestAllowed, supported, remaining := installedSDK()
	for _, goversion := range supported {
		gvbs := bs
		gvbs.Goversion = goversion
		success := fileExists(filepath.Join(gvbs.storeDir(), "recordnumber"))
		p := request{gvbs, "", pageIndex}.link()
		goversionLinks = append(goversionLinks, goversionLink{goversion, p, success, true, p == xlink})
	}
	for _, goversion := range remaining {
		gvbs := bs
		gvbs.Goversion = goversion
		success := fileExists(filepath.Join(gvbs.storeDir(), "recordnumber"))
		p := request{gvbs, "", pageIndex}.link()
		goversionLinks = append(goversionLinks, goversionLink{goversion, p, success, false, p == xlink})
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
		tbs := bs
		tbs.Goos = target.Goos
		tbs.Goarch = target.Goarch
		success := fileExists(filepath.Join(tbs.storeDir(), "recordnumber"))
		p := request{tbs, "", pageIndex}.link()
		targetLinks = append(targetLinks, targetLink{target.Goos, target.Goarch, p, success, p == xlink})
	}

	pkgGoDevURL := "https://pkg.go.dev/" + path.Join(bs.Mod+"@"+bs.Version, bs.Dir[1:]) + "?tab=doc"

	resp := <-c

	var filesizeGz string
	if br == nil {
		br = &buildResult{buildSpec: bs}
	} else {
		if info, err := os.Stat(filepath.Join(bs.storeDir(), "binary.gz")); err == nil {
			filesizeGz = fmt.Sprintf("%.1f MB", float64(info.Size())/(1024*1024))
		}
	}

	prependDir := xreq.Dir
	if prependDir == "/" {
		prependDir = ""
	}

	var newerText, newerURL string
	if xreq.Goversion != newestAllowed && newestAllowed != "" && xreq.Version != resp.LatestVersion && resp.LatestVersion != "" {
		newerText = "A newer version of both this module and the Go toolchain is available"
	} else if xreq.Version != resp.LatestVersion && resp.LatestVersion != "" {
		newerText = "A newer version of this module is available"
	} else if xreq.Goversion != newestAllowed && newestAllowed != "" {
		newerText = "A newer Go toolchain version available"
	}
	if newerText != "" {
		nbs := bs
		nbs.Version = resp.LatestVersion
		nbs.Goversion = newestAllowed
		newerURL = request{nbs, "", pageIndex}.link()
	}

	favicon := "/favicon.ico"
	if output != "" {
		favicon = "/favicon-error.png"
	} else if br.Sum == "" {
		favicon = "/favicon-building.png"
	}
	args := map[string]interface{}{
		"Favicon":                favicon,
		"Success":                br.Sum != "",
		"Sum":                    br.Sum,
		"Req":                    xreq,             // eg "/" or "/cmd/x"
		"DirAppend":              xreq.appendDir(), // eg "" or "cmd/x/"
		"DirPrepend":             prependDir,       // eg "" or /cmd/x"
		"GoversionLinks":         goversionLinks,
		"TargetLinks":            targetLinks,
		"Mod":                    resp,
		"GoProxy":                config.GoProxy,
		"DownloadFilename":       xreq.downloadFilename(),
		"PkgGoDevURL":            pkgGoDevURL,
		"GobuildVersion":         gobuildVersion,
		"GobuildPlatform":        gobuildPlatform,
		"VerifierKey":            config.VerifierKey,
		"GobuildsOrgVerifierKey": gobuildsOrgVerifierKey,
		"NewerText":              newerText,
		"NewerURL":               newerURL,

		// Whether we will do SSE request for updates.
		"InProgress": br.Sum == "" && output == "",

		// Non-empty on failure.
		"Output": output,

		// Below only meaningful when "success".
		"Filesize":   fmt.Sprintf("%.1f MB", float64(br.Filesize)/(1024*1024)),
		"FilesizeGz": filesizeGz,
	}

	if br.Sum == "" {
		w.Header().Set("Cache-Control", "no-store")
	}

	if err := buildTemplate.Execute(w, args); err != nil {
		failf(w, "%w: executing template: %v", errServer, err)
	}
}

func readGzipFile(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fgz, err := gzip.NewReader(f); err != nil {
		return nil, err
	} else {
		return io.ReadAll(fgz)
	}
}
