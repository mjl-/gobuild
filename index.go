package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/mjl-/bstore"
)

// todo: handle errors
func lookupSum(ctx context.Context, bs buildSpec) (sum *buildSum) {
	err := database.Read(ctx, func(tx *bstore.Tx) error {
		q := bstore.QueryTx[Result](tx)
		q.FilterNonzero(bs.result())
		q.FilterEqual("Stripped", bs.Stripped)
		q.FilterGreater("TreeRecordID", ID(0))
		result, err := q.Get()
		if err != nil {
			return err
		}

		rec := TreeRecord{ID: result.TreeRecordID}
		if err := tx.Get(&rec); err != nil {
			return err
		}
		sum = &rec.Sum
		return nil
	})
	if err != nil && err != bstore.ErrAbsent {
		logger(ctx).Error("looking for record for buildspec", "err", err, "buildspec", bs)
		return nil
	}
	return sum
}

// serveIndex serves the HTML page for a build/result, that has either failed or is
// pending or has succeeded.
func serveIndex(w http.ResponseWriter, r *http.Request, bs buildSpec, result *Result, record *TreeRecord) {
	ctx := r.Context()
	xreq := request{bs, nil, pageIndex}
	if record != nil {
		xreq.Sum = &record.Sum
	}
	xlink := xreq.link()

	gv, err := parseGoVersion(bs.Goversion)
	if err != nil {
		failf(w, r, "%w: parsing go version: %v", errServer, err)
		return
	}

	type versionLink struct {
		Version string
		URLPath string
		Sum     *buildSum
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
		defer logPanic(logger(ctx))
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
		mreq, err := http.NewRequestWithContext(ctx, "GET", u, nil)
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
		if resp.StatusCode != http.StatusOK {
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
		for s := range strings.SplitSeq(string(buf), "\n") {
			if s != "" {
				vbs := bs
				vbs.Version = s
				sum := lookupSum(ctx, vbs)
				p := request{vbs, sum, pageIndex}.link()
				link := versionLink{s, p, sum, p == xlink}
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

	// Non-empty output means we'll serve the error page instead of doing a SSE request for events.
	var output string
	if result != nil && record == nil {
		bl := BuildLog{ID: result.ID}
		if err := database.Get(ctx, &bl); err != nil {
			failf(w, r, "%w: get build log: %v", errServer, err)
			return
		} else if output, err = decompressGzip(bl.Data); err != nil {
			failf(w, r, "%w: decompress build log: %v", errServer, err)
			return
		}
	}

	// Construct links to other goversions, targets.
	type goversionLink struct {
		Goversion string
		URLPath   string
		Sum       *buildSum
		Supported bool
		Active    bool
	}
	goversionLinks := []goversionLink{}
	newestAllowed, supported, remaining := listSDK(ctx)
	for _, goversion := range supported {
		gvbs := bs
		gvbs.Goversion = goversion
		sum := lookupSum(ctx, gvbs)
		p := request{gvbs, sum, pageIndex}.link()
		goversionLinks = append(goversionLinks, goversionLink{goversion, p, sum, true, p == xlink})
	}
	for _, goversion := range remaining {
		gvbs := bs
		gvbs.Goversion = goversion
		sum := lookupSum(ctx, gvbs)
		p := request{gvbs, sum, pageIndex}.link()
		goversionLinks = append(goversionLinks, goversionLink{goversion, p, sum, false, p == xlink})
	}

	type targetLink struct {
		Goos    string
		Goarch  string
		URLPath string
		Sum     *buildSum
		Active  bool
	}
	targetLinks := []targetLink{}
	for _, target := range targets.get() {
		tbs := bs
		tbs.Goos = target.Goos
		tbs.Goarch = target.Goarch
		sum := lookupSum(ctx, tbs)
		p := request{tbs, sum, pageIndex}.link()
		targetLinks = append(targetLinks, targetLink{target.Goos, target.Goarch, p, sum, p == xlink})
	}

	type variantLink struct {
		Variant string // "default" or "stripped"
		Title   string // Displayed on hover in UI.
		URLPath string
		Sum     *buildSum
		Active  bool
	}
	var variantLinks []variantLink
	addVariant := func(v, title string, stripped bool) {
		vbs := bs
		vbs.Stripped = stripped
		sum := lookupSum(ctx, vbs)
		p := request{vbs, sum, pageIndex}.link()
		variantLinks = append(variantLinks, variantLink{v, title, p, sum, p == xlink})
	}
	addVariant("default", "", false)
	addVariant("stripped", "Symbol table and debug information stripped, reducing binary size.", true)

	pkgGoDevURL := "https://pkg.go.dev/" + path.Join(bs.Mod+"@"+bs.Version, bs.Dir[1:]) + "?tab=doc"

	resp := <-c

	var filesize string
	if record != nil {
		filesize = fmt.Sprintf("%.1f MB", float64(record.FileSize)/(1024*1024))
	}
	var filesizeGz string
	if result != nil && result.FileSizeGz > 0 {
		filesizeGz = fmt.Sprintf("%.1f MB", float64(result.FileSizeGz)/(1024*1024))
	}

	var sum buildSum
	if record != nil {
		sum = record.Sum
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
		newerText = "A newer Go toolchain version is available"
	}
	if newerText != "" {
		nbs := bs
		nbs.Version = resp.LatestVersion
		nbs.Goversion = newestAllowed
		sum := lookupSum(ctx, nbs)
		newerURL = request{nbs, sum, pageIndex}.link()
	}

	favicon := "/favicon.ico"
	if output != "" {
		favicon = "/favicon-error.png"
	} else if result == nil {
		favicon = "/favicon-building.png"
	}
	args := map[string]any{
		"Favicon":                favicon,
		"Success":                record != nil,
		"Sum":                    sum,
		"Req":                    xreq,             // eg "/" or "/cmd/x"
		"DirAppend":              xreq.appendDir(), // eg "" or "cmd/x/"
		"DirPrepend":             prependDir,       // eg "" or /cmd/x"
		"GoversionLinks":         goversionLinks,
		"TargetLinks":            targetLinks,
		"VariantLinks":           variantLinks,
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

		// Non-empty on failure.
		"Output": output,

		// Below only meaningful when "success".
		"Filesize":   filesize,
		"FilesizeGz": filesizeGz,

		"GO19CONCURRENTCOMPILATION": gv.major == 1 && gv.minor < 26,
	}

	if record == nil {
		w.Header().Set("Cache-Control", "no-store")
	}

	if err := buildTemplate.Execute(w, args); err != nil {
		failf(w, r, "%w: executing build template: %w", errServer, err)
	}
}

func decompressGzip(data []byte) (string, error) {
	var b strings.Builder
	if fgz, err := gzip.NewReader(bytes.NewReader(data)); err != nil {
		return "", err
	} else if _, err := io.Copy(&b, fgz); err != nil {
		return "", err
	} else {
		return b.String(), nil
	}
}
