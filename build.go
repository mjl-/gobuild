package main

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"
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
			slog.Info("bad client", "user-agent", r.UserAgent(), "remoteaddr", r.RemoteAddr, "hostname", hostname)
			statusfail(http.StatusForbidden, w, "Your request matched a list of clients/networks with known bad behaviour. Please respect the robots.txt (no crawling that triggers builds!) and be kind. Contact the admins to get access again.")
			return true
		}
	}

	if !config.Ratelimit.Enabled {
		return false
	}

	ipstr, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		slog.Info("parsing remote address for rate limit", "err", err, "addr", r.RemoteAddr)
		return false
	}
	ip := net.ParseIP(ipstr)
	if ip == nil {
		slog.Info("parsing remote ip address for rate limit", "err", err, "addr", r.RemoteAddr, "ipstr", ipstr)
		return false
	}

	_, supported, _ := listSDK()
	limiter := &limiterOld
	goage := "old"
	metric := metricClientBuildRequestsLimitedOld
	if slices.Contains(supported, bs.Goversion) {
		limiter = &limiterRecent
		goage = "recent"
		metric = metricClientBuildRequestsLimitedRecent
	}
	if !limiter.Add(ip, time.Now(), 1) {
		metric.Inc()
		slog.Info("build requested rejected through rate limit", "remoteaddr", r.RemoteAddr, "user-agent", r.UserAgent(), "goage", goage, "goversion", bs.Goversion)
		statusfail(http.StatusTooManyRequests, w, fmt.Sprintf("Your IP or its neighbourhood has requested too many builds (for %s Go versions) in a short period. Please slow down, and try again later. Contact the admins to get the rate limit loosened.", goage))
		return true
	}

	return false
}

func serveBuild(w http.ResponseWriter, r *http.Request, req request) {
	// Resolve "latest" goversion with a redirect.
	if req.Goversion == "latest" {
		if newestAllowed, _, _ := listSDK(); newestAllowed == "" {
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
		if info, err := resolveModuleVersion(r.Context(), req.Mod, req.Version); err != nil {
			failf(w, "resolving latest for module: %w", err)
		} else {
			mreq := req
			mreq.Version = info.Version
			http.Redirect(w, r, mreq.link(), http.StatusTemporaryRedirect)
		}
		return
	}

	// See if we have a completed build, and handle it.
	if _, br, _, failed, err := (serverOps{}).lookupResult(r.Context(), req.buildSpec); err != nil {
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
			// Just a sanity check that we aren't removing successful build results.
			if _, err := os.Stat(filepath.Join(dir, "recordnumber")); err == nil {
				failf(w, "%w: directory with failed build contains recordnumber-file", errServer)
				return
			}

			tmpdir := dir + ".remove"
			if err := os.Rename(dir, tmpdir); err != nil {
				failf(w, "%w: moving away directory with log of failed build: %v", errServer, err)
				return
			}
			if err := os.RemoveAll(tmpdir); err != nil {
				failf(w, "%w: removing path of failed build: %s", errServer, err)
				return
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

	if handleBadClient(w, r, req.buildSpec) {
		return
	}

	// No build yet, we need one. Keep in mind that another build could finish between
	// the checks above and below. This isn't a problem: preparing a build never hurts,
	// and builds go through the coordinator, which always first checks if a build has
	// completed.

	ctx := r.Context()

	// We always immediately attempt to get the files for a build. This checks with the
	// goproxy that the module and package exist, and seems like it has a chance to
	// compile.
	if err := prepareBuild(ctx, req.buildSpec); err != nil {
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
	registerBuild(req.buildSpec, "", eventc, parseRemoteAddr(r.RemoteAddr))

	switch req.Page {
	case pageEvents:
		// For the events endpoint, we send updates as they come in.
		flusher, ok := w.(http.Flusher)
		if !ok {
			slog.Error("ResponseWriter not a http.Flusher")
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
