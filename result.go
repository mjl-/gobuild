package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
)

func serveResult(w http.ResponseWriter, r *http.Request, req request) {
	ctx := r.Context()

	var br *BuildResult

	result, record, binaryPresent, err := serverOps{}.lookupResult(ctx, req.buildSpec)
	if err != nil {
		failf(w, r, "%w: lookup record: %v", errServer, err)
		return
	} else if record == nil || !binaryPresent {
		if handleBadClient(w, r, req.buildSpec) {
			return
		}

		// Attempt to build.
		if err := prepareBuild(ctx, req.buildSpec); err != nil {
			failf(w, r, "preparing build: %w", err)
			return
		}

		var exp *BuildResult
		if record != nil {
			exp = &BuildResult{*result, *record}
		}
		eventc := make(chan buildUpdate, 100)
		registerBuild(logger(ctx), req.buildSpec, exp, eventc, remoteIP(r))
		defer unregisterBuild(req.buildSpec, eventc)

	loop:
		for {
			select {
			case <-ctx.Done():
				return
			case update := <-eventc:
				if !update.done {
					continue
				}
				if update.err != nil {
					failf(w, r, "build failed: %w", update.err)
					return
				}
				br = update.buildResult
				break loop
			}
		}
	} else {
		br = &BuildResult{*result, *record}
	}

	if br.TreeRecord.Sum != *req.Sum {
		http.NotFound(w, r)
		return
	}

	switch req.Page {
	case pageLog:
		serveLog(w, r, result.ID)
	case pageDownloadRedirect:
		link := request{req.buildSpec, &br.TreeRecord.Sum, pageDownload}.link()
		http.Redirect(w, r, link, http.StatusTemporaryRedirect)
	case pageDownload:
		p := filepath.Join(config.DataDir, "binaries", fmt.Sprintf("%d.gz", record.ID))
		f, err := os.Open(p)
		if err != nil {
			failf(w, r, "%w: open binary: %v", errServer, err)
			return
		}
		defer f.Close()
		serveGzipFile(w, r, f)
	case pageDownloadGz:
		p := filepath.Join(config.DataDir, "binaries", fmt.Sprintf("%d.gz", record.ID))
		http.ServeFile(w, r, p)
	case pageRecord:
		if msg, err := br.Record().Pack(); err != nil {
			failf(w, r, "%w: packing record: %v", errServer, err)
		} else {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write(msg) // nothing to do for errors
		}
	case pageIndex:
		serveIndex(w, r, req.buildSpec, &br.Result, &br.TreeRecord)
	default:
		failf(w, r, "%w: unknown page %v", errServer, req.Page)
	}
}
