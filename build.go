package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
)

func serveBuild(w http.ResponseWriter, r *http.Request, req request) {
	// Resolve "latest" goversion with a redirect.
	if req.Goversion == "latest" {
		if newestAllowed, _, _ := installedSDK(); newestAllowed == "" {
			failf(w, "no supported go toolchains available: %w", errServer)
		} else {
			vreq := req
			vreq.Goversion = newestAllowed
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
		case pageRetry:
			// We'll move away the directory with the failed build, remove it, and redirect
			// user to the index page so a new build is triggered.
			dir := req.buildSpec.storeDir()
			tmpdir := dir + ".remove"
			if err := os.Rename(dir, tmpdir); err != nil {
				failf(w, "%w: moving away directory with log of failed build: %v", errServer, err)
				return
			}
			// Just a sanity check that we aren't removing successful build results.
			if _, err := os.Stat(filepath.Join(dir, "recordnumber")); err == nil {
				os.Rename(tmpdir, dir) // Attemp to restore order...
				failf(w, "%w: directory with failed build contains recordnumber-file", errServer)
				return
			}
			paths := []string{
				filepath.Join(tmpdir, "log.gz"),
				tmpdir,
			}
			for _, p := range paths {
				if err := os.Remove(p); err != nil {
					failf(w, "%w: removing path of failed build: %s", errServer, err)
					return
				}
			}
			req.Page = pageIndex
			http.Redirect(w, r, req.link(), http.StatusSeeOther)
		case pageLog:
			serveLog(w, r, filepath.Join(req.storeDir(), "log.gz"))
		case pageIndex:
			serveIndex(w, r, req.buildSpec, nil)
		default:
			failf(w, "build failed, see index page for details")
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
