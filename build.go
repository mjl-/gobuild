package main

import (
	"fmt"
	"net"
	"net/http"
	"slices"
	"time"

	"github.com/mjl-/bstore"
)

// handleBadClient is called for builds triggered through the web interface (not
// the /tlog lookup endpoint), to check if the client is bad (e.g. a crawler
// triggering builds by following all the links at the bottom of a build/result
// page).
func handleBadClient(w http.ResponseWriter, r *http.Request, bs buildSpec) bool {
	metricClientBuildRequests.Inc()
	for _, cp := range config.BadClients {
		if hostname, ok := cp.Match(r); ok {
			metricClientBuildRequestsBad.Inc()
			logger(r.Context()).Info("bad client", "user-agent", r.UserAgent(), "remoteip", remoteIP(r), "hostname", hostname)
			statusfail(r.Context(), http.StatusForbidden, w, "Your request matched a list of clients/networks with known bad behaviour. Please respect the robots.txt (no crawling that triggers builds!) and be kind. Contact the admins to get access again.")
			return true
		}
	}

	if !config.Ratelimit.Enabled {
		return false
	}

	ip := remoteIP(r)

	_, supported, _ := listSDK(r.Context())
	limiter := &limiterOld
	goage := "old"
	metric := metricClientBuildRequestsLimitedOld
	if slices.Contains(supported, bs.Goversion) {
		limiter = &limiterRecent
		goage = "recent"
		metric = metricClientBuildRequestsLimitedRecent
	}
	if !limiter.Add(net.IP(ip.AsSlice()), time.Now(), 1) {
		metric.Inc()
		logger(r.Context()).Info("build requested rejected through rate limit", "remoteip", ip, "user-agent", r.UserAgent(), "goage", goage, "goversion", bs.Goversion)
		statusfail(r.Context(), http.StatusTooManyRequests, w, fmt.Sprintf("Your IP or its neighbourhood has requested too many builds (for %s Go versions) in a short period. Please slow down, and try again later. Contact the admins to get the rate limit loosened.", goage))
		return true
	}

	return false
}

func serveBuild(w http.ResponseWriter, r *http.Request, req request) {
	ctx := r.Context()
	log := logger(ctx)

	var resolvedLatest bool
	nreq := req

	// Resolve "latest" goversion with a redirect.
	if req.Goversion == "latest" {
		if newestAllowed, _, _ := listSDK(ctx); newestAllowed == "" {
			failf(w, r, "no supported go toolchains available: %w", errServer)
			return
		} else {
			nreq.Goversion = newestAllowed
			resolvedLatest = true
		}
	}

	// Resolve "latest" module version with a redirect.
	if req.Version == "latest" {
		if info, err := resolveModuleVersion(ctx, req.Mod, req.Version); err != nil {
			failf(w, r, "resolving latest for module: %w", err)
			return
		} else {
			nreq.Version = info.Version
			resolvedLatest = true
		}
	}

	if resolvedLatest {
		http.Redirect(w, r, nreq.link(), http.StatusTemporaryRedirect)
		return
	}

	// See if we have a completed build, and handle it.
	if result, record, _, err := (serverOps{}).lookupResult(ctx, req.buildSpec); err != nil {
		failf(w, r, "%w: lookup record: %v", errServer, err)
		return
	} else if record != nil {
		// Redirect to the permanent URLs that include the hash.
		link := request{result.buildSpec(), &record.Sum, req.Page}.link()
		http.Redirect(w, r, link, http.StatusTemporaryRedirect)
		return
	} else if result != nil {
		// Show failed build to user, for the pages where that works.
		switch req.Page {
		case pageRetry:
			err := database.Write(ctx, func(tx *bstore.Tx) error {
				bl := BuildLog{ID: result.ID}
				return tx.Delete(&bl, result)
			})
			if err != nil {
				failf(w, r, "%w: deleting previous result: %v", errServer, err)
				return
			}
			req.Page = pageIndex
			http.Redirect(w, r, req.link(), http.StatusSeeOther)
		case pageLog:
			serveLog(w, r, result.ID)
		case pageIndex:
			serveIndex(w, r, req.buildSpec, result, nil)
		default:
			failf(w, r, "build failed, see index page for details")
		}
		return
	}

	if handleBadClient(w, r, req.buildSpec) {
		return
	}

	// No build yet, we need one. Keep in mind that another build could finish between
	// the checks above and below. This isn't a problem: preparing a build never hurts,
	// and builds go through the coordinator, which always first checks if a build has
	// completed.

	// We always immediately attempt to get the files for a build. This checks with the
	// goproxy that the module and package exist, and seems like it has a chance to
	// compile.
	if err := prepareBuild(ctx, req.buildSpec); err != nil {
		failf(w, r, "preparing build: %w", err)
		return
	}

	// We serve the index page immediately. It can make an SSE-request to the /events
	// endpoint to register a request for the build and to receive updates. Pages other
	// than /events will block until a build completes.
	if req.Page == pageIndex {
		serveIndex(w, r, req.buildSpec, nil, nil)
		return
	}

	eventc := make(chan buildUpdate, 100)
	registerBuild(logger(ctx), req.buildSpec, nil, eventc, remoteIP(r))
	defer unregisterBuild(req.buildSpec, eventc)

	switch req.Page {
	case pageEvents:
		// For the events endpoint, we send updates as they come in.
		flusher, ok := w.(http.Flusher)
		if !ok {
			log.Error("ResponseWriter not a http.Flusher")
			failf(w, r, "%w: implementation limitation: cannot stream updates", errServer)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
			return
		}
		flusher.Flush()

		for {
			select {
			case <-ctx.Done():
				return
			case update := <-eventc:
				_, err := w.Write(update.msg)
				flusher.Flush()
				if update.done || err != nil {
					return
				}
			}
		}

	default:
		// For all other pages, we just wait until the build completes.
		for {
			select {
			case <-ctx.Done():
				return
			case update := <-eventc:
				if !update.done {
					continue
				}

				if req.Page == pageLog && update.result != nil {
					serveLog(w, r, update.result.ID)
					return
				}

				if update.err != nil {
					failf(w, r, "build failed: %w", update.err)
					return
				}

				// Redirect to the permanent URLs that include the hash.
				link := request{update.result.buildSpec(), &update.treeRecord.Sum, req.Page}.link()
				http.Redirect(w, r, link, http.StatusTemporaryRedirect)
				return
			}
		}
	}
}
