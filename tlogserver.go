package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/mjl-/gobuild/internal/sumdb"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/mod/sumdb/note"
	"golang.org/x/mod/sumdb/tlog"
)

type serverOps struct {
	signer note.Signer
}

var _ sumdb.ServerOps = serverOps{}

type hashReader struct{}

func (h hashReader) ReadHashes(indexes []int64) ([]tlog.Hash, error) {
	hashes := make([]tlog.Hash, len(indexes))

	for i, index := range indexes {
		if _, err := hashesFile.ReadAt(hashes[i][:], index*tlog.HashSize); err != nil {
			return nil, err
		}
	}
	return hashes, nil
}

func observeOp(rerr *error, t0 time.Time, errorCounter prometheus.Counter, histo prometheus.Histogram) {
	histo.Observe(time.Since(t0).Seconds())
	if *rerr != nil {
		errorCounter.Inc()
	}
}

// Signed returns the signed hash of the latest tree.
func (s serverOps) Signed(ctx context.Context) (result []byte, rerr error) {
	defer observeOp(&rerr, time.Now(), metricTlogOpsSignedErrors, metricTlogOpsSignedDuration)

	if n, err := treeSize(); err != nil {
		return nil, err
	} else if h, err := tlog.TreeHash(n, hashReader{}); err != nil {
		return nil, err
	} else {
		text := tlog.FormatTree(tlog.Tree{N: n, Hash: h})
		return note.Sign(&note.Note{Text: string(text)}, s.signer)
	}
}

// ReadRecords returns the content for the n records id through id+n-1.
func (s serverOps) ReadRecords(ctx context.Context, id, n int64) (results [][]byte, rerr error) {
	// log.Printf("server: ReadRecords %d %d", id, n)
	defer observeOp(&rerr, time.Now(), metricTlogOpsReadrecordsErrors, metricTlogOpsReadrecordsDuration)

	if n <= 0 {
		return nil, fmt.Errorf("bad n")
	}

	all := make([]byte, n*diskRecordSize)
	if _, err := recordsFile.ReadAt(all, id*diskRecordSize); err != nil {
		return nil, fmt.Errorf("reading records: %v", err)
	}
	result := make([][]byte, n)
	for i := int64(0); i < n; i++ {
		buf := all[:diskRecordSize]
		all = all[diskRecordSize:]
		size := int(buf[0])<<8 | int(buf[1])
		result[i] = buf[2 : 2+size]
	}
	return result, nil
}

// Lookup looks up a record for the given key,
// returning the record ID.
// Returns os.ErrNotExist to cause a http 404 response.
func (s serverOps) Lookup(ctx context.Context, key string) (results int64, rerr error) {
	// log.Printf("server: Lookup %q", key)
	defer observeOp(&rerr, time.Now(), metricTlogOpsLookupErrors, metricTlogOpsLookupDuration)

	bs, err := parseBuildSpec(key)
	if err != nil {
		return -1, os.ErrNotExist
	}

	if bs.Goversion == "latest" {
		bs.Goversion, _, _ = listSDK()
	}

	if !targets.valid(bs.Goos + "/" + bs.Goarch) {
		return -1, os.ErrNotExist
	}

	// Resolve module version. Could be a git hash.
	info, err := resolveModuleVersion(ctx, bs.Mod, bs.Version)
	if err != nil {
		return -1, err
	}
	bs.Version = info.Version

	p := filepath.Join(bs.storeDir(), "recordnumber")
	if buf, err := os.ReadFile(p); err != nil {
		if os.IsNotExist(err) {
			return lookupBuild(ctx, bs)
		}
		return -1, err
	} else {
		return strconv.ParseInt(string(buf), 10, 64)
	}
}

// Attempt to read a result.
// For a successful build, the recordNumber and buildResult are returned.
// For a failed build (not in the tlog), failed will be true.
// For an absent build, err wil be nil.
// For other errors, err will be set.
func (s serverOps) lookupResult(ctx context.Context, bs buildSpec) (recordNumber int64, br *buildResult, failed bool, err error) {
	p := filepath.Join(bs.storeDir(), "recordnumber")
	if buf, err := os.ReadFile(p); err != nil {
		if !os.IsNotExist(err) {
			return -1, nil, false, err
		}
		lp := filepath.Join(bs.storeDir(), "log.gz")
		if _, err := os.Stat(lp); err == nil {
			return -1, nil, true, nil
		} else if os.IsNotExist(err) {
			return -1, nil, false, nil
		} else {
			return 0, nil, false, err
		}
	} else if num, err := strconv.ParseInt(string(buf), 10, 64); err != nil {
		return -1, nil, false, err
	} else if records, err := s.ReadRecords(ctx, num, 1); err != nil {
		return -1, nil, false, err
	} else if record, err := parseRecord(records[0]); err != nil {
		return -1, nil, false, err
	} else {
		return num, record, false, nil
	}
}

// Returns os.ErrNotExist (not wrapped) if the fault is in the request, to cause a http 404 response for the lookup.
func lookupBuild(ctx context.Context, bs buildSpec) (int64, error) {
	// Before attempting to build, check we don't have a failed build already.
	if _, err := os.Stat(filepath.Join(bs.storeDir(), "log.gz")); err == nil {
		return -1, os.ErrNotExist
	}

	// note: We don't check for abusive clients. The problems are more likely web
	// crawlers, they won't find these endpoints. Allowing lookups through the lookup
	// endpoint keeps automated downloads, e.g. of updates, working from networks that
	// may also host bad crawlers.

	// Attempt to build.
	if err := prepareBuild(bs); err != nil {
		if errors.Is(err, errBadGoversion) || errors.Is(err, os.ErrNotExist) || errors.Is(err, errNotExist) || errors.Is(err, errBadModule) || errors.Is(err, errBadVersion) {
			return -1, os.ErrNotExist
		}
		return -1, fmt.Errorf("preparing build: %w", err)
	}

	eventc := make(chan buildUpdate, 100)
	registerBuild(bs, eventc)

	for {
		select {
		case <-ctx.Done():
			unregisterBuild(bs, eventc)
			return -1, ctx.Err()
		case update := <-eventc:
			if !update.done {
				continue
			}
			unregisterBuild(bs, eventc)
			if update.err != nil {
				// todo: turn some errors into "file not found".
				return -1, fmt.Errorf("build failed: %s", update.err)
			}
			return update.recordNumber, nil
		}
	}
}

// ReadTileData reads the content of tile t.
// It is only invoked for hash tiles (t.L ≥ 0).
func (s serverOps) ReadTileData(ctx context.Context, t tlog.Tile) (results []byte, rerr error) {
	// log.Printf("server: ReadTileData %#v", t)
	defer observeOp(&rerr, time.Now(), metricTlogOpsReadtiledataErrors, metricTlogOpsReadtiledataDuration)

	return tlog.ReadTileData(t, hashReader{})
}
