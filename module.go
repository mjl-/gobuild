package main

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

func serveModules(w http.ResponseWriter, r *http.Request) {
	defer observePage("module", time.Now())

	mod := r.URL.Path[1:]
	if strings.HasSuffix(mod, "/") {
		mod = strings.TrimRight(mod, "/")
		http.Redirect(w, r, "/"+mod, http.StatusTemporaryRedirect)
		return
	}

	info, err := resolveModuleLatest(r.Context(), config.GoProxy, mod)
	if err != nil {
		failf(w, "resolving latest for module: %w", err)
		return
	}

	goversion, err := ensureMostRecentSDK()
	if err != nil {
		failf(w, "ensuring most recent goversion: %w", err)
		return
	}
	gobin, err := ensureGobin(goversion)
	if err != nil {
		failf(w, "%w", err)
		return
	}

	modDir, getOutput, err := ensureModule(goversion, gobin, mod, info.Version)
	if err != nil {
		failf(w, "error fetching module from goproxy: %w\n\n# output from go get:\n%s", err, string(getOutput))
		return
	}

	goos, goarch := autodetectTarget(r)

	bs := buildSpec{mod, info.Version, "", goos, goarch, goversion}

	mainDirs, err := listMainPackages(gobin, modDir)
	if err != nil {
		failf(w, "listing main packages in module: %w", err)
		return
	} else if len(mainDirs) == 0 {
		failf(w, "no main packages in module")
		return
	} else if len(mainDirs) == 1 {
		bs.Dir = "/" + filepath.ToSlash(mainDirs[0])
		link := request{bs, "", pageIndex}.link()
		http.Redirect(w, r, link, http.StatusTemporaryRedirect)
		return
	}

	type mainPkg struct {
		Link string
		Name string
	}
	mainPkgs := []mainPkg{}
	for _, md := range mainDirs {
		bs.Dir = "/" + filepath.ToSlash(md)
		link := request{bs, "", pageIndex}.link()
		if md == "" {
			md = "/"
		}
		mainPkgs = append(mainPkgs, mainPkg{link, md})
	}
	args := struct {
		Favicon        string
		Module         string
		Version        string
		Mains          []mainPkg
		GobuildVersion string
	}{
		"/favicon.ico",
		bs.Mod,
		bs.Version,
		mainPkgs,
		gobuildVersion,
	}
	if err := moduleTemplate.Execute(w, args); err != nil {
		failf(w, "%w: executing template: %v", errServer, err)
	}
}

func listMainPackages(gobin string, modDir string) ([]string, error) {
	goproxy := true
	cgo := true
	cmd := makeCommand(goproxy, modDir, cgo, nil, gobin, "list", "-f", "{{.Name}} {{ .Dir }}", "./...")
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w\n\n# output from go list:\n%s\n\nstderr:\n%s", err, output, stderr.String())
	}
	r := []string{}
	for _, s := range strings.Split(string(output), "\n") {
		if !strings.HasPrefix(s, "main ") {
			continue
		}
		s = s[len("main "):]
		if s == modDir {
			r = append(r, "")
			continue
		}
		if !strings.HasPrefix(s, modDir+string(filepath.Separator)) {
			continue
		}
		s = s[len(modDir)+1:]
		if s != "" {
			s += string(filepath.Separator)
		}
		r = append(r, s)
	}
	return r, nil
}

func autodetectTarget(r *http.Request) (goos, goarch string) {
	ua := r.Header.Get("User-Agent")
	ua = strings.ToLower(ua)

	// Because the targets list we range over is sorted by popularity, we
	// are more likely to guess (partially) right.
	match := ""
	for _, t := range targets.get() {
		m0 := strings.Contains(ua, t.Goos)
		if !m0 && t.Goos == "darwin" {
			m0 = strings.Contains(ua, "macos") || strings.Contains(ua, "macintosh") || strings.Contains(ua, "mac os x")
		}
		m1 := strings.Contains(ua, t.Goarch)
		if !m1 && t.Goarch == "amd64" {
			m1 = strings.Contains(ua, "x86_64") || strings.Contains(ua, "x86-64") || strings.Contains(ua, "x64; ") || strings.Contains(ua, "win64") || strings.Contains(ua, "wow64")
		}
		if !m1 && t.Goarch == "386" {
			m1 = strings.Contains(ua, "i686") || strings.Contains(ua, "x86; ") || strings.Contains(ua, "win32")
		}
		if !m0 && !m1 {
			continue
		}
		m := ""
		if m0 {
			m += t.Goos
		}
		if m1 {
			m += t.Goarch
		}
		if len(m) > len(match) {
			goos, goarch = t.Goos, t.Goarch
			match = m
		}
	}
	if goos == "" || goarch == "" {
		t := targets.get()[0]
		goos, goarch = t.Goos, t.Goarch
	}
	return
}
