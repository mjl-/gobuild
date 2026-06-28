package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mjl-/gobuild/internal/atime"
)

// Total size of the binaries (sum of the sizes of the gzipped binary files) in the
// binaries/ data directory. Initialized at startup by walking that directory.
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

var binaryCacheCleanupBusy atomic.Bool

// Walk binaries/ in the data directory, removing gzipped binary files of at least
// "reclaim" bytes (based on access time, i.e. least recently used).
//
// If reclaim is <= 0, no files are removed. This can be used to determine the
// current cache size by walking the files on disk.
func binaryCacheCleanup(reclaim int64) error {
	if binaryCacheCleanupBusy.Swap(true) {
		return nil
	}
	defer binaryCacheCleanupBusy.Swap(false)

	slog.Info("walking binaries/ directory for cached gzipped binary files possible storage reclaim", "reclaim", reclaim)

	// We'll walk the result/ dir and keep track of all gzipped binary files.
	type Binary struct {
		path  string
		size  int64
		atime time.Time
	}
	var binaries []Binary

	dir := filepath.Join(config.DataDir, "binaries")

	l, err := os.ReadDir(dir)
	if err != nil {
		slog.Error("cleanup binaries by size: walking", "err", err, "path", dir)
		return nil
	}
	for _, e := range l {
		if !strings.HasSuffix(e.Name(), ".gz") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if fi, err := e.Info(); err != nil {
			slog.Error("cleanup binaries by size: stat", "err", err, "path", path)
		} else if t, err := atime.Get(fi); err != nil {
			slog.Error("cleanup binaries by size: get access time", "err", err, "path", path)
		} else {
			binaries = append(binaries, Binary{path, fi.Size(), t})
		}
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
		slog.Info("removing least recently used cached gzipped binary", "path", b.path, "size", b.size, "atime", b.atime)
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

	return nil
}

func cleanupBinariesAtime(atimeAge time.Duration) {
	dir := filepath.Join(config.DataDir, "binaries")
	l, err := os.ReadDir(dir)
	if err != nil {
		slog.Error("cleanup binaries: walking", "err", err, "path", dir)
		return
	}
	for _, e := range l {
		if !strings.HasSuffix(e.Name(), ".gz") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if fi, err := e.Info(); err != nil {
			slog.Error("cleanup binaries: stat", "err", err, "path", path)
		} else if t, err := atime.Get(fi); err != nil {
			slog.Error("cleanup binaries: get access time", "err", err, "path", path)
		} else if time.Since(t) > atimeAge {
			if err := os.Remove(path); err != nil {
				slog.Error("cleanup binaries: removing old binary", "err", err, "path", path)
			} else {
				slog.Info("cleanup binaries: removed aging binary", "path", path)
			}
		}
	}
}
