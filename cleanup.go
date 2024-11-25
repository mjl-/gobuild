package main

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func cleanupBinariesAtime(atimeAge time.Duration) {
	dir := filepath.Join(config.DataDir, "result")
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			log.Printf("cleanup binaries: walking %q: %s, skipping", path, err)
			return nil
		}
		if d.Name() != "binary.gz" {
			return nil
		}
		if fi, err := d.Info(); err != nil {
			log.Printf("cleanup binaries: stat %q: %s, skipping", path, err)
		} else if t, err := atime(fi); err != nil {
			log.Printf("cleanup binaries: get access time for %q: %s, skipping", path, err)
		} else if time.Since(t) > atimeAge {
			if err := os.Remove(path); err != nil {
				log.Printf("cleanup binaries: removing old binary %s: %v", path, err)
			} else {
				log.Printf("cleanup binaries: removed aging binary %s", path)
			}
		}
		return nil
	})
	if err != nil {
		log.Printf("walking result directory for old binary.gz files: %s", err)
	}
}

func cleanupGoBuildCache() {
	log.Printf("clearing go build cache")

	goversion, err := ensureMostRecentSDK()
	if err != nil {
		log.Printf("cleaning up go build cache: ensuring most recent toolchain while resolving module version: %v", err)
		return
	}
	gobin, err := ensureGobin(goversion.String())
	if err != nil {
		log.Printf("cleaning up go build cache: ensuring go version is available while resolving module version: %v", err)
		return
	}

	cmd := makeCommand(goversion.String(), false, emptyDir, false, nil, gobin, "clean", "-cache")
	output, err := cmd.Output()
	if err != nil {
		log.Printf("running go clean -cache: %s (%q)", err, strings.TrimSpace(string(output)))
	}
}
