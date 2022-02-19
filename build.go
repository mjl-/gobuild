package main

import (
	"log"
	"net/http"
	"path/filepath"
)

func serveBuild(w http.ResponseWriter, r *http.Request, req request) {
	// Resolve "latest" goversion with a redirect.
	if req.Goversion == "latest" {
		if supported, _ := installedSDK(); len(supported) == 0 {
			http.Error(w, "503 - No supported Go toolchains available", http.StatusServiceUnavailable)
		} else {
			vreq := req
			vreq.Goversion = supported[0]
			http.Redirect(w, r, vreq.link(), http.StatusTemporaryRedirect)
		}
		return
	}

	// Resolve "latest" module version with a redirect.
	if req.Version == "latest" {
		if info, err := resolveModuleLatest(r.Context(), config.GoProxy, req.Mod); err != nil {
			failf(w, "resolving latest for module: %w", err)
		} else {
			mreq := req
			mreq.Version = info.Version
			http.Redirect(w, r, mreq.link(), http.StatusTemporaryRedirect)
		}
		return
	}

	// See if we have a completed build, and handle it.
	if _, br, failed, err := (serverOps{}).lookupResult(r.Context(), req.buildSpec); err != nil {
		failf(w, "%w: lookup record: %v", errServer, err)
		return
	} else if br != nil {
		// Redirect to the permanent URLs that include the hash.
		link := request{br.buildSpec, br.Sum, req.Page}.link()
		http.Redirect(w, r, link, http.StatusTemporaryRedirect)
		return
	} else if failed {
		// Show failed build to user, for the pages where that works.
		switch req.Page {
		case pageLog:
			serveLog(w, r, filepath.Join(req.storeDir(), "log.gz"))
		case pageIndex:
			serveIndex(w, r, req.buildSpec, nil)
		default:
			http.Error(w, "400 - Bad Request - build failed, see index page for details", http.StatusBadRequest)
		}
		return
	}

	// No build yet, we need one. Keep in mind that another build could finish between
	// the checks above and below. This isn't a problem: preparing a build never hurts,
	// and builds go through the coordinator, which always first checks if a build has
	// completed.

	// We always immediately attempt to get the files for a build build. This checks
	// with the goproxy that the module and package exist, and seems like it has a
	// chance to compile.
	if err := prepareBuild(req.buildSpec); err != nil {
		failf(w, "preparing build: %w", err)
		return
	}

	// We serve the index page immediately. It makes an SSE-request to the /events
	// endpoint to register a request for the build and to receive updates.
	// Pages other than /events will block until a build completes.
	if req.Page == pageIndex {
		serveIndex(w, r, req.buildSpec, nil)
		return
	}

	eventc := make(chan buildUpdate, 100)
	registerBuild(req.buildSpec, eventc)

	ctx := r.Context()

	switch req.Page {
	case pageEvents:
		// For the events endpoint, we send updates as they come in.
		flusher, ok := w.(http.Flusher)
		if !ok {
			log.Println("ResponseWriter not a http.Flusher")
			failf(w, "%w: implementation limitation: cannot stream updates", errServer)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
			return
		}
		flusher.Flush()

	loop:
		for {
			select {
			case <-ctx.Done():
				break loop
			case update := <-eventc:
				_, err := w.Write(update.msg)
				flusher.Flush()
				if update.done || err != nil {
					break loop
				}
			}
		}
		unregisterBuild(req.buildSpec, eventc)

	default:
		// For all other pages, we just wait until the build completes.
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

				if req.Page == pageLog {
					serveLog(w, r, filepath.Join(req.storeDir(), "log.gz"))
					return
				}

				if update.err != nil {
					failf(w, "build failed: %w", update.err)
					return
				}

				// Redirect to the permanent URLs that include the hash.
				link := request{update.result.buildSpec, update.result.Sum, req.Page}.link()
				http.Redirect(w, r, link, http.StatusTemporaryRedirect)
				return
			}
		}
	}
}
