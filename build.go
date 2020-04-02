package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func serveBuild(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "405 - Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	req, hint, ok := parseRequest(r.URL.Path)
	if !ok {
		if hint != "" {
			http.Error(w, fmt.Sprintf("404 - File Not Found\n\n%s\n", hint), http.StatusNotFound)
		} else {
			http.NotFound(w, r)
		}
		return
	}
	defer observePage("build "+req.Page.String(), time.Now())

	// Resolve "latest" goversion with a redirect.
	if req.Goversion == "latest" {
		supported, _ := installedSDK()
		if len(supported) == 0 {
			http.Error(w, "503 - no go supported toolchains available", http.StatusServiceUnavailable)
			return
		}
		goversion := supported[0]
		vreq := req
		vreq.Goversion = goversion
		http.Redirect(w, r, vreq.urlPath(), http.StatusTemporaryRedirect)
		return
	}

	// Resolve "latest" module version with a redirect.
	if req.Version == "latest" {
		info, err := resolveModuleLatest(r.Context(), req.Mod)
		if err != nil {
			failf(w, "resolving latest for module: %w", err)
			return
		}

		mreq := req
		mreq.Version = info.Version
		http.Redirect(w, r, mreq.urlPath(), http.StatusTemporaryRedirect)
		return
	}

	lpath := filepath.Join(config.DataDir, req.storeDir())

	// If build.json exists, we have a successful build.
	bf, err := os.Open(filepath.Join(lpath, "build.json"))
	if err == nil {
		defer bf.Close()
	}
	var buildResult buildJSON
	if err == nil {
		err = json.NewDecoder(bf).Decode(&buildResult)
	}
	if err != nil && !os.IsNotExist(err) {
		failf(w, "%w: reading build.json: %v", errServer, err)
		return
	}
	if err == nil {
		// Redirect to the permanent URLs that include the hash.
		rreq := req
		rreq.Sum = "0" + base64.RawURLEncoding.EncodeToString(buildResult.SHA256[:20])
		http.Redirect(w, r, rreq.urlPath(), http.StatusTemporaryRedirect)
		return
	}

	// If log.gz exists, we have a failed build.
	_, err = os.Stat(filepath.Join(lpath, "log.gz"))
	if err != nil && !os.IsNotExist(err) {
		failf(w, "%w: stat path: %v", errServer, err)
		return
	}
	if err == nil {
		// Show failed build to user, for the pages where that works.
		switch req.Page {
		case pageLog:
			serveLog(w, r, filepath.Join(lpath, "log.gz"))
		case pageIndex:
			serveIndex(w, r, req, nil)
		default:
			http.Error(w, "400 - Bad Request - build failed, see index page for details", http.StatusBadRequest)
		}
		return
	}

	// No build yet, we need one.

	// We always attempt to set up the build. This checks with the goproxy that the
	// module and package exist, and seems like it has a chance to compile.
	err = prepareBuild(req)
	if err != nil {
		failf(w, "preparing build: %w", err)
		return
	}

	// We serve the index page immediately. It makes an SSE-request to the /events
	// endpoint to register a request for the build and to receive updates.
	if req.Page == pageIndex {
		serveIndex(w, r, req, nil)
		return
	}

	eventc := make(chan buildUpdate, 100)
	registerBuild(req, eventc)

	ctx := r.Context()

	switch req.Page {
	case pageEvents:
		flusher, ok := w.(http.Flusher)
		if !ok {
			log.Println("ResponseWriter not a http.Flusher")
			failf(w, "%w: implementation limitation: cannot stream updates", errServer)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		_, err := w.Write([]byte(": keepalive\n\n"))
		if err != nil {
			return
		}
		flusher.Flush()

	loop:
		for {
			select {
			case <-ctx.Done():
				break loop
			case update := <-eventc:
				_, err = w.Write(update.msg)
				flusher.Flush()
				if update.done || err != nil {
					break loop
				}
			}
		}
		unregisterBuild(req, eventc)
	default:
		for {
			select {
			case <-ctx.Done():
				unregisterBuild(req, eventc)
				return
			case update := <-eventc:
				if !update.done {
					continue
				}
				unregisterBuild(req, eventc)

				if req.Page == pageLog {
					serveLog(w, r, filepath.Join(lpath, "log.gz"))
					return
				}

				if update.err != nil {
					failf(w, "build failed: %w", update.err)
					return
				}

				// Redirect to the permanent URLs that include the hash.
				rreq := req
				rreq.Sum = "0" + base64.RawURLEncoding.EncodeToString(update.result.SHA256[:20])
				http.Redirect(w, r, rreq.urlPath(), http.StatusTemporaryRedirect)
				return
			}
		}
	}
}

func acceptsGzip(r *http.Request) bool {
	s := r.Header.Get("Accept-Encoding")
	t := strings.Split(s, ",")
	for _, e := range t {
		e = strings.TrimSpace(e)
		tt := strings.Split(e, ";")
		if len(tt) > 1 && t[1] == "q=0" {
			continue
		}
		if tt[0] == "gzip" {
			return true
		}
	}
	return false
}
