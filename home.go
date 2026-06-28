package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

func readInstanceNotes(ctx context.Context) string {
	if config.InstanceNotesFile == "" {
		return ""
	}

	buf, err := os.ReadFile(config.InstanceNotesFile)
	if err != nil {
		logger(ctx).Error("reading instance notes failed, skipping", "err", err)
	}
	return string(buf)
}

func serveHome(w http.ResponseWriter, r *http.Request) {
	defer observePage("home", time.Now())

	m := r.FormValue("m")
	if m != "" {
		http.Redirect(w, r, "/"+m, http.StatusTemporaryRedirect)
		return
	}

	recentBuilds.Lock()
	recentLinks := slices.Clone(recentBuilds.links)
	recentBuilds.Unlock()

	// Reverse order for recentLinks most recent first.
	n := len(recentLinks)
	for i := range n / 2 {
		j := n - 1 - i
		recentLinks[i], recentLinks[j] = recentLinks[j], recentLinks[i]
	}

	var args = struct {
		Favicon         string
		Recents         []string
		VerifierKey     string
		GobuildVersion  string
		GobuildPlatform string
		InstanceNotes   string
	}{
		"favicon.ico",
		recentLinks,
		config.VerifierKey,
		gobuildVersion,
		gobuildPlatform,
		readInstanceNotes(r.Context()),
	}
	if err := homeTemplate.Execute(w, args); err != nil {
		failf(w, r, "%w: executing home template: %w", errServer, err)
	}
}

type specResponseWriter struct {
	Start               time.Time
	Request             *http.Request
	Code                int
	http.ResponseWriter // Pass through Header().
}

func newSpecResponseWriter(w http.ResponseWriter, r *http.Request) *specResponseWriter {
	return &specResponseWriter{Start: time.Now(), Request: r, ResponseWriter: w}
}

func (w *specResponseWriter) Log(ctx context.Context) {
	logger(ctx).Info("http response", "method", w.Request.Method, "path", w.Request.URL.Path, "statuscode", w.Code, "duration", time.Since(w.Start))
}

func (w *specResponseWriter) Status(statusCode int) {
	if w.Code == 0 {
		w.Code = statusCode
	}
}

func (w *specResponseWriter) Write(buf []byte) (int, error) {
	w.Status(http.StatusOK)
	return w.ResponseWriter.Write(buf)
}

func (w *specResponseWriter) WriteHeader(statusCode int) {
	w.Status(statusCode)
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *specResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	} else {
		slog.Error("ResponseWriter not a http.Flusher")
	}
}

func serveSpec(w http.ResponseWriter, r *http.Request) {
	defer observePage("spec", time.Now())

	ctx := r.Context()

	// Log request as it comes in, and the result when done.
	logger(ctx).Info("http request", "method", r.Method, "path", r.URL.Path)
	sw := newSpecResponseWriter(w, r)
	w = sw
	defer sw.Log(ctx)

	t := strings.Split(r.URL.Path[1:], "/")
	if !strings.Contains(t[0], ".") {
		http.NotFound(w, r)
		return
	}

	if !strings.Contains(r.URL.Path, "@") {
		if r.Method != "GET" {
			http.Error(w, "405 - Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		if !checkAllowedRespond(ctx, w, r.URL.Path[1:]) {
			return
		}

		serveModules(w, r)
		return
	}

	// If last part contains an "@" and no part before it does, and last part doesn't
	// have a slash, we'll assume a path like /github.com/mjl-/sherpa@v0.6.0 and
	// redirect to a path with guessed goos/goarch and latest goversion.
	if mod, version, _ := strings.Cut(r.URL.Path[1:], "@"); mod != "" && version != "" && !strings.Contains(version, "@") && !strings.Contains(version, "/") {
		goversion, err := ensureMostRecentSDK(ctx)
		if err != nil {
			failf(w, r, "ensuring most recent sdk: %w", err)
			return
		}

		// Resolve module version. Could be a git hash.
		info, err := resolveModuleVersion(ctx, mod, version)
		if err != nil {
			failf(w, r, "resolving module version: %w", err)
			return
		}
		version = info.Version

		goos, goarch := autodetectTarget(r)
		bs := buildSpec{mod, version, "/", goos, goarch, goversion.String(), false}

		req := request{bs, nil, pageIndex}
		http.Redirect(w, r, req.link(), http.StatusTemporaryRedirect)
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
	if req.Page != pageRetry && r.Method != "GET" || req.Page == pageRetry && r.Method != "POST" {
		http.Error(w, "405 - Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if !checkAllowedRespond(ctx, w, req.Mod) {
		return
	}

	// If we have a result for this request, don't look up the module version, which
	// involves executing a command.
	result, _, _, err := (serverOps{}).lookupResult(ctx, req.buildSpec)
	if err != nil {
		failf(w, r, "%w: resolving module version: %v", errServer, err)
		return
	}
	if result == nil {
		// Try resolving the module version. It could be a git hash.
		info, err := resolveModuleVersion(ctx, req.Mod, req.Version)
		if err != nil {
			failf(w, r, "resolving module version: %w", err)
			return
		}
		if req.Version != info.Version {
			req.Version = info.Version
			http.Redirect(w, r, req.link(), http.StatusTemporaryRedirect)
			req.link()
			return
		}
	}

	what := "build"
	if req.Sum != nil {
		what = "result"
	}
	defer observePage(what+" "+req.Page.String(), time.Now())
	if req.Sum == nil {
		serveBuild(w, r, req)
	} else {
		serveResult(w, r, req)
	}
}

func checkAllowedRespond(ctx context.Context, w http.ResponseWriter, module string) bool {
	if len(config.ModulePrefixes) == 0 {
		return true
	}
	for _, prefix := range config.ModulePrefixes {
		if strings.HasPrefix(module, prefix) {
			return true
		}
	}
	statusfail(ctx, http.StatusForbidden, w, "403 - Module path not allowed")
	return false
}
