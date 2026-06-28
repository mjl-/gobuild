package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
)

func serveResult(w http.ResponseWriter, r *http.Request, req request) {
	ctx := r.Context()

	result, treeRecord, binaryPresent, err := serverOps{}.lookupResult(ctx, req.buildSpec)
	if err != nil {
		failf(w, r, "%w: lookup record: %v", errServer, err)
		return
	} else if treeRecord == nil || !binaryPresent {
		if handleBadClient(w, r, req.buildSpec) {
			return
		}

		// Attempt to build.
		if err := prepareBuild(ctx, req.buildSpec); err != nil {
			failf(w, r, "preparing build: %w", err)
			return
		}

		var exp *BuildResult
		if treeRecord != nil {
			exp = &BuildResult{*result, *treeRecord}
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
				result, treeRecord = update.result, update.treeRecord
				break loop
			}
		}
	}

	if treeRecord.Sum != *req.Sum {
		http.NotFound(w, r)
		return
	}

	switch req.Page {
	case pageLog:
		serveLog(w, r, result.ID)
	case pageDownloadRedirect:
		link := request{req.buildSpec, &treeRecord.Sum, pageDownload}.link()
		http.Redirect(w, r, link, http.StatusTemporaryRedirect)
	case pageDownload:
		p := filepath.Join(config.DataDir, "binaries", fmt.Sprintf("%d.gz", treeRecord.ID))
		f, err := os.Open(p)
		if err != nil {
			failf(w, r, "%w: open binary: %v", errServer, err)
			return
		}
		defer f.Close()
		serveGzipFile(w, r, f)
	case pageDownloadGz:
		p := filepath.Join(config.DataDir, "binaries", fmt.Sprintf("%d.gz", treeRecord.ID))
		http.ServeFile(w, r, p)
	case pageRecord:
		br := BuildResult{*result, *treeRecord}
		if msg, err := br.Record().Pack(); err != nil {
			failf(w, r, "%w: packing record: %v", errServer, err)
		} else {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write(msg) // nothing to do for errors
		}
	case pageIndex:
		serveIndex(w, r, req.buildSpec, result, treeRecord)
	default:
		failf(w, r, "%w: unknown page %v", errServer, req.Page)
	}
}
