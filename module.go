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

	if r.Method != "GET" {
		http.Error(w, "405 - Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/m/" {
		m := r.FormValue("m")
		if m == "" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/m/"+m, http.StatusTemporaryRedirect)
		return
	}

	mod := r.URL.Path[len("/m/"):]
	if strings.HasSuffix(mod, "/") {
		mod = strings.TrimRight(mod, "/")
		http.Redirect(w, r, "/m/"+mod, http.StatusTemporaryRedirect)
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
	gobin := filepath.Join(config.SDKDir, goversion, "bin", "go")
	if !filepath.IsAbs(gobin) {
		gobin = filepath.Join(workdir, gobin)
	}

	modDir, getOutput, err := ensureModule(gobin, mod, info.Version)
	if err != nil {
		failf(w, "error fetching module from goproxy: %w\n\n# output from go get:\n%s", err, string(getOutput))
		return
	}

	goos, goarch := autodetectTarget(r)

	mainDirs, err := listMainPackages(gobin, modDir)
	if err != nil {
		failf(w, "listing main packages in module: %w", err)
		return
	}
	if len(mainDirs) == 0 {
		failf(w, "no main packages in module")
		return
	}
	if len(mainDirs) == 1 {
		req := request{
			Mod:       mod,
			Version:   info.Version,
			Dir:       filepath.ToSlash(mainDirs[0]),
			Goos:      goos,
			Goarch:    goarch,
			Goversion: goversion,
			Page:      pageIndex,
		}
		http.Redirect(w, r, req.urlPath(), http.StatusTemporaryRedirect)
		return
	}

	type mainPkg struct {
		Link string
		Name string
	}
	mainPkgs := []mainPkg{}
	for _, md := range mainDirs {
		md = filepath.ToSlash(md)
		req := request{
			Mod:       mod,
			Version:   info.Version,
			Dir:       md,
			Goos:      goos,
			Goarch:    goarch,
			Goversion: goversion,
			Page:      pageIndex,
		}
		if md == "" {
			md = "/"
		}
		mainPkgs = append(mainPkgs, mainPkg{req.urlPath(), md})
	}
	args := struct {
		Module         string
		Version        string
		Mains          []mainPkg
		GobuildVersion string
	}{
		mod,
		info.Version,
		mainPkgs,
		gobuildVersion,
	}
	err = moduleTemplate.Execute(w, args)
	if err != nil {
		failf(w, "%w: executing template: %v", errServer, err)
	}
}

func listMainPackages(gobin string, modDir string) ([]string, error) {
	cgo := true
	cmd := makeCommand(cgo, gobin, "list", "-f", "{{.Name}} {{ .Dir }}", "./...")
	cmd.Dir = modDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w\n\n# output from go list:\n%s", err, output)
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
