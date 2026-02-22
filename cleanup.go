package main

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Total size of the binaries (sum of the sizes of the binary.gz files) in the
// result/ data directory. Initialized at startup, either from
// "result/binary-cache-size.txt", or by walking the result/ directory.
//
// Only used for config.BinaryCacheSizeMax, not for CleanupBinariesAccessTimeAge.
var binaryCache struct {
	size int64
	sync.Mutex
}

// n can be negative.
// If a cleanup is needed, the return value is > 0, indicating the number of bytes
// that need to be reclaimed.
func binaryCacheSizeAdd(n int64) int64 {
	binaryCache.Lock()
	defer binaryCache.Unlock()

	binaryCache.size += n

	if config.binaryCacheSizeMax > 0 && binaryCache.size > config.binaryCacheSizeMax {
		return binaryCache.size - config.binaryCacheSizeMax*9/10
	}
	return 0
}

func binaryCacheSizeWrite() error {
	binaryCache.Lock()
	defer binaryCache.Unlock()

	p := filepath.Join(config.DataDir, "result", "binary-cache-size.txt")
	if err := os.WriteFile(p, fmt.Appendf(nil, "%d\n", binaryCache.size), 0o644); err != nil {
		return fmt.Errorf("writing result/binary-cache-size.txt: %v", err)
	}
	return nil
}

var binaryCacheCleanupBusy atomic.Bool

// Walk result/ in the data directory, removing binary.gz files of at least
// "reclaim" bytes (based on access time, i.e. least recently used).
//
// Finally, the new remaining total size is written to result/binary-cache-size.txt in
// the data directory.
//
// If reclaim is <= 0, no files are removed. This can be used to determine the
// current cache size by walking the files on disk.
func binaryCacheCleanup(reclaim int64) error {
	if binaryCacheCleanupBusy.Swap(true) {
		return nil
	}
	defer binaryCacheCleanupBusy.Swap(false)

	slog.Info("walking result/ directory for cached binary.gz files possible storage reclaim", "reclaim", reclaim)

	// We'll walk the result/ dir and keep track of all binary.gz files.
	type Binary struct {
		path  string
		size  int64
		atime time.Time
	}
	var binaries []Binary

	dir := filepath.Join(config.DataDir, "result")
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			slog.Error("cleanup binaries by size: walking", "err", err, "path", path)
			return nil
		}
		if d.Name() != "binary.gz" {
			return nil
		}
		if fi, err := d.Info(); err != nil {
			slog.Error("cleanup binaries by size: stat", "err", err, "path", path)
		} else if t, err := atime(fi); err != nil {
			slog.Error("cleanup binaries by size: get access time", "err", err, "path", path)
		} else {
			binaries = append(binaries, Binary{path, fi.Size(), t})
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walking result directory for old binary.gz files: %v", err)
	}

	// Sort binaries, oldest first.
	slices.SortFunc(binaries, func(a, b Binary) int {
		return a.atime.Compare(b.atime)
	})

	// Keep removing files until there are no bytes we need to reclaim.
	var reclaimed int64
	for reclaim > 0 && len(binaries) > 0 {
		b := binaries[0]
		binaries = binaries[1:]
		slog.Info("removing least recently used cached binary.gz", "path", b.path, "size", b.size, "atime", b.atime)
		if err := os.Remove(b.path); err != nil {
			return fmt.Errorf("removing cached %s to reach max size: %v", b.path, err)
		}
		reclaim -= b.size
		reclaimed += b.size
	}

	// Set new size based on remaining files we found.
	var size int64
	for _, b := range binaries {
		size += b.size
	}
	binaryCache.Lock()
	binaryCache.size = size
	binaryCache.Unlock()

	slog.Info("binary cache size after cleanup", "size", size, "reclaimed", reclaimed)

	// And write the binary-cache-size.txt file.
	if err := binaryCacheSizeWrite(); err != nil {
		return err
	}

	return nil
}

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
