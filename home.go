package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

func readInstanceNotes() string {
	if config.InstanceNotesFile == "" {
		return ""
	}

	buf, err := os.ReadFile(config.InstanceNotesFile)
	if err != nil {
		slog.Error("reading instance notes failed, skipping", "err", err)
	}
	return string(buf)
}

func serveHome(w http.ResponseWriter, r *http.Request) {
	defer observePage("home", time.Now())

	if r.URL.Path == "/" {
		if r.Method != "GET" {
			http.Error(w, "405 - Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

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
			readInstanceNotes(),
		}
		if err := homeTemplate.Execute(w, args); err != nil {
			failf(w, "%w: executing home template: %w", errServer, err)
		}
		return
	}

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

		if !checkAllowedRespond(w, r.URL.Path[1:]) {
			return
		}
		serveModules(w, r)
		return
	}

	// If last part contains an "@" and no part before it does, and last part doesn't
	// have a slash, we'll assume a path like /github.com/mjl-/sherpa@v0.6.0 and
	// redirect to a path with guessed goos/goarch and latest goversion.
	if mod, version, _ := strings.Cut(r.URL.Path[1:], "@"); mod != "" && version != "" && !strings.Contains(version, "@") && !strings.Contains(version, "/") {
		goversion, err := ensureMostRecentSDK()
		if err != nil {
			failf(w, "ensuring most recent sdk: %w", err)
			return
		}

		// Resolve module version. Could be a git hash.
		info, err := resolveModuleVersion(r.Context(), mod, version)
		if err != nil {
			failf(w, "resolving module version: %w", err)
			return
		}
		version = info.Version

		goos, goarch := autodetectTarget(r)
		bs := buildSpec{mod, version, "/", goos, goarch, goversion.String(), false}

		req := request{bs, "", pageIndex}
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
	if !checkAllowedRespond(w, req.Mod) {
		return
	}

	// Resolve module version. Could be a git hash.
	info, err := resolveModuleVersion(r.Context(), req.Mod, req.Version)
	if err != nil {
		failf(w, "resolving module version: %w", err)
		return
	}
	if req.Version != info.Version {
		req.Version = info.Version
		http.Redirect(w, r, req.link(), http.StatusTemporaryRedirect)
		req.link()
		return
	}

	what := "build"
	if req.Sum != "" {
		what = "result"
	}
	defer observePage(what+" "+req.Page.String(), time.Now())
	if req.Sum == "" {
		serveBuild(w, r, req)
	} else {
		serveResult(w, r, req)
	}
}

func checkAllowedRespond(w http.ResponseWriter, module string) bool {
	if len(config.ModulePrefixes) == 0 {
		return true
	}
	for _, prefix := range config.ModulePrefixes {
		if strings.HasPrefix(module, prefix) {
			return true
		}
	}
	statusfailf(http.StatusForbidden, w, "403 - Module path not allowed")
	return false
}
