package main

import (
	"net/http"
	"strings"
	"time"
)

func serveHome(w http.ResponseWriter, r *http.Request) {
	defer observePage("home", time.Now())

	if r.Method != "GET" {
		http.Error(w, "405 - Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	if r.URL.Path == "/" {
		m := r.FormValue("m")
		if m != "" {
			http.Redirect(w, r, "/"+m, http.StatusTemporaryRedirect)
			return
		}

		recentBuilds.Lock()
		recentLinks := append([]string{}, recentBuilds.links...)
		recentBuilds.Unlock()

		// Reverse order for recentLinks most recent first.
		n := len(recentLinks)
		for i := 0; i < n/2; i++ {
			j := n - 1 - i
			recentLinks[i], recentLinks[j] = recentLinks[j], recentLinks[i]
		}

		var args = struct {
			Recents        []string
			VerifierKey    string
			GobuildVersion string
		}{
			recentLinks,
			config.VerifierKey,
			gobuildVersion,
		}
		if err := homeTemplate.Execute(w, args); err != nil {
			failf(w, "%w: executing template: %v", errServer, err)
		}
		return
	}

	t := strings.Split(r.URL.Path[1:], "/")
	if !strings.Contains(t[0], ".") {
		http.NotFound(w, r)
		return
	}

	if !strings.Contains(r.URL.Path, "@") {
		serveModules(w, r)
		return
	}
	if t[len(t)-1] == "" {
		t = t[:len(t)-1]
	}
	if isSum(t[len(t)-1]) || (len(t) > 1 && isSum(t[len(t)-2])) {
		serveResult(w, r)
	} else {
		serveBuild(w, r)
	}
}
