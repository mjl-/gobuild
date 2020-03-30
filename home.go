package main

import (
	"net/http"
	"time"
)

func serveHome(w http.ResponseWriter, r *http.Request) {
	defer observePage("home", time.Now())
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != "GET" {
		http.Error(w, "405 - Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	recentBuilds.Lock()
	recents := recentBuilds.paths
	recentBuilds.Unlock()

	var args = struct {
		Recents []string
	}{
		recents,
	}
	err := homeTemplate.Execute(w, args)
	if err != nil {
		failf(w, "%w: executing template: %v", errServer, err)
	}
}
