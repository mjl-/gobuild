package main

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func cleanupBinariesAtime(atimeAge time.Duration) {
	dir := filepath.Join(config.DataDir, "result")
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			slog.Error("cleanup binaries: walking", "err", err, "path", path)
			return nil
		}
		if d.Name() != "binary.gz" {
			return nil
		}
		if fi, err := d.Info(); err != nil {
			slog.Error("cleanup binaries: stat", "err", err, "path", path)
		} else if t, err := atime(fi); err != nil {
			slog.Error("cleanup binaries: get access time", "err", err, "path", path)
		} else if time.Since(t) > atimeAge {
			if err := os.Remove(path); err != nil {
				slog.Error("cleanup binaries: removing old binary", "err", err, "path", path)
			} else {
				slog.Info("cleanup binaries: removed aging binary", "path", path)
			}
		}
		return nil
	})
	if err != nil {
		slog.Error("walking result directory for old binary.gz files", "err", err)
	}
}

func cleanupGoBuildCache() {
	slog.Debug("clearing go build cache")

	goversion, err := ensureMostRecentSDK()
	if err != nil {
		slog.Error("cleaning up go build cache: ensuring most recent toolchain while resolving module version", "err", err)
		return
	}
	gobin, err := ensureGobin(goversion.String())
	if err != nil {
		slog.Error("cleaning up go build cache: ensuring go version is available while resolving module version", "err", err)
		return
	}

	cmd := makeCommand(goversion.String(), false, emptyDir, false, nil, gobin, "clean", "-cache")
	output, err := cmd.CombinedOutput()
	if err != nil {
		slog.Error("running go clean -cache", "err", err, "output", strings.TrimSpace(string(output)))
	}
}
