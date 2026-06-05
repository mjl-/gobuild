package main

import (
	"net/http"
	"os"
	"path/filepath"
)

func serveResult(w http.ResponseWriter, r *http.Request, req request) {
	storeDir := req.storeDir()

	_, br, binaryPresent, failed, err := serverOps{}.lookupResult(r.Context(), req.buildSpec)
	if err != nil {
		failf(w, r, "%w: lookup record: %v", errServer, err)
		return
	} else if failed {
		http.NotFound(w, r)
		return
	} else if br == nil || !binaryPresent {
		if handleBadClient(w, r, req.buildSpec) {
			return
		}

		ctx := r.Context()

		// Attempt to build.
		if err := prepareBuild(ctx, req.buildSpec); err != nil {
			failf(w, r, "preparing build: %w", err)
			return
		}

		var expSum string
		if br != nil {
			expSum = br.Sum
		}
		eventc := make(chan buildUpdate, 100)
		registerBuild(logger(r.Context()), req.buildSpec, expSum, eventc, remoteIP(r))

	loop:
		for {
			select {
			case <-ctx.Done():
				unregisterBuild(req.buildSpec, eventc)
				return
			case update := <-eventc:
				if !update.done {
					continue
				}
				unregisterBuild(req.buildSpec, eventc)
				if update.err != nil {
					failf(w, r, "build failed: %w", update.err)
					return
				}
				r := *update.result
				br = &r
				break loop
			}
		}
	}

	if br.Sum != req.Sum {
		http.NotFound(w, r)
		return
	}

	switch req.Page {
	case pageLog:
		serveLog(w, r, filepath.Join(storeDir, "log.gz"))
	case pageDownloadRedirect:
		link := request{req.buildSpec, br.Sum, pageDownload}.link()
		http.Redirect(w, r, link, http.StatusTemporaryRedirect)
	case pageDownload:
		p := filepath.Join(storeDir, "binary.gz")
		f, err := os.Open(p)
		if err != nil {
			failf(w, r, "%w: open binary: %v", errServer, err)
			return
		}
		defer f.Close()
		serveGzipFile(w, r, p, f)
	case pageDownloadGz:
		p := filepath.Join(storeDir, "binary.gz")
		http.ServeFile(w, r, p)
	case pageRecord:
		if msg, err := br.packRecord(); err != nil {
			failf(w, r, "%w: packing record: %v", errServer, err)
		} else {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write(msg) // nothing to do for errors
		}
	case pageIndex:
		serveIndex(w, r, req.buildSpec, br)
	default:
		failf(w, r, "%w: unknown page %v", errServer, req.Page)
	}
}
