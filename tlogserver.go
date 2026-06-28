package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/mjl-/gobuild/internal/sumdb"

	"github.com/mjl-/bstore"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/mod/sumdb/note"
	"golang.org/x/mod/sumdb/tlog"
)

type serverOps struct {
	signer note.Signer
}

var _ sumdb.ServerOps = serverOps{}

func treeSize(ctx context.Context) (int64, error) {
	n, err := bstore.QueryDB[TreeRecord](ctx, database).Count()
	return int64(n), err
}

type hashReader struct {
	tx *bstore.Tx
}

func (h hashReader) ReadHashes(indexes []int64) ([]tlog.Hash, error) {
	hashes := make([]tlog.Hash, len(indexes))

	for i, index := range indexes {
		hash := TreeHash{ID: Number(index).ID()}
		if err := h.tx.Get(&hash); err != nil {
			return nil, fmt.Errorf("get hash for index %d: %w", index, err)
		}
		hashes[i] = hash.Sum
	}

	return hashes, nil
}

func observeOp(ctx context.Context, op string, rerr *error, t0 time.Time, errorCounter prometheus.Counter, histo prometheus.Histogram) {
	delta := time.Since(t0)
	histo.Observe(delta.Seconds())
	if *rerr != nil {
		errorCounter.Inc()
		logger(ctx).Error("serverop error", "op", op, "err", *rerr, "duration", delta)
	}
}

// Signed returns the signed hash of the latest tree.
func (s serverOps) Signed(ctx context.Context) (result []byte, rerr error) {
	defer observeOp(ctx, "signed", &rerr, time.Now(), metricTlogOpsSignedErrors, metricTlogOpsSignedDuration)

	err := database.Read(ctx, func(tx *bstore.Tx) error {
		n, err := bstore.QueryTx[TreeRecord](tx).Count()
		if err != nil {
			return fmt.Errorf("get tree size: %v", err)
		} else if h, err := tlog.TreeHash(int64(n), hashReader{tx}); err != nil {
			return fmt.Errorf("calculating tree hash: %v", err)
		} else {
			text := tlog.FormatTree(tlog.Tree{N: int64(n), Hash: h})
			result, err = note.Sign(&note.Note{Text: string(text)}, s.signer)
			return err
		}
	})
	return result, err
}

// ReadRecords returns the content for the n records num through num+n-1.
func (s serverOps) ReadRecords(ctx context.Context, num, n int64) (results [][]byte, rerr error) {
	// log.Printf("server: ReadRecords %d %d", num, n)
	defer observeOp(ctx, "readrecord", &rerr, time.Now(), metricTlogOpsReadrecordsErrors, metricTlogOpsReadrecordsDuration)

	if n <= 0 {
		return nil, fmt.Errorf("bad n")
	}

	records := make([][]byte, 0, n)
	err := database.Read(ctx, func(tx *bstore.Tx) error {
		end := num + n
		for i := num; i < end; i++ {
			record := TreeRecord{ID: Number(i).ID()}
			if err := tx.Get(&record); err != nil {
				return fmt.Errorf("read record from database: %w", err)
			}
			result := Result{ID: record.ResultID}
			if err := tx.Get(&result); err != nil {
				return fmt.Errorf("read result from database: %w", err)
			}
			buf, err := BuildResult{result, record}.Record().Pack()
			if err != nil {
				return fmt.Errorf("pack record: %w", err)
			}
			records = append(records, buf)
		}
		return nil
	})
	return records, err
}

// Lookup looks up a record for the given key,
// returning the record number.
// Returns os.ErrNotExist to cause a http 404 response.
func (s serverOps) Lookup(ctx context.Context, key string) (recordNum int64, rerr error) {
	// log.Printf("server: Lookup %q", key)
	defer func() {
		// Don't cause error metric to go up for bad requests or requested builds that
		// don't exist.
		err := rerr
		if err != nil && errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		observeOp(ctx, "lookup", &err, time.Now(), metricTlogOpsLookupErrors, metricTlogOpsLookupDuration)
	}()

	bs, err := parseBuildSpec(key)
	if err != nil {
		return -1, os.ErrNotExist
	}

	if bs.Goversion == "latest" {
		bs.Goversion, _, _ = listSDK(ctx)
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

	result, record, _, err := s.lookupResult(ctx, bs)
	if err != nil {
		return -1, fmt.Errorf("lookup result in database: %w", err)
	} else if result == nil {
		return s.lookupBuild(ctx, bs, ctx.Value(keyRequest{}).(*http.Request))
	} else if record == nil {
		return -1, fmt.Errorf("no successful build available: %w", err)
	}
	return int64(record.ID.Number()), nil
}

// Attempt to read a result, both successful and failed.
// If no result is present, err is nil and result & record are nil.
// For a failed build, result is non-nil and record is nil.
// For a successful build, record is non-nil.
func (s serverOps) lookupResult(ctx context.Context, bs buildSpec) (result *Result, record *TreeRecord, binaryPresent bool, rerr error) {
	err := database.Read(ctx, func(tx *bstore.Tx) error {
		q := bstore.QueryTx[Result](tx)
		q.FilterNonzero(bs.result())
		q.FilterEqual("Stripped", bs.Stripped)
		r, err := q.Get()
		if err != nil {
			return err
		}
		result = &r
		if r.TreeRecordID > 0 {
			rec := TreeRecord{ID: r.TreeRecordID}
			if err := tx.Get(&rec); err != nil {
				return fmt.Errorf("get tree record: %w", err)
			}
			record = &rec
		}
		return nil
	})
	if err != nil && errors.Is(err, bstore.ErrAbsent) {
		return nil, nil, false, nil
	} else if err != nil {
		return nil, nil, false, err
	}

	var present bool
	if record != nil {
		p := filepath.Join(config.DataDir, "binaries", fmt.Sprintf("%d.gz", record.ID))
		if _, err := os.Stat(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, nil, false, fmt.Errorf("stat binary: %w", err)
		} else {
			present = err == nil
		}
	}

	return result, record, present, nil
}

// Returns os.ErrNotExist (not wrapped) if the fault is in the request, to
// cause a http 404 response for the lookup.
func (s serverOps) lookupBuild(ctx context.Context, bs buildSpec, r *http.Request) (int64, error) {
	// note: We don't check for abusive clients. The problems are more likely web
	// crawlers, they won't find these endpoints. Allowing lookups through the lookup
	// endpoint keeps automated downloads, e.g. of updates, working from networks that
	// may also host bad crawlers.

	// Attempt to build.
	if err := prepareBuild(ctx, bs); err != nil {
		if errors.Is(err, errBadGoversion) || errors.Is(err, os.ErrNotExist) || errors.Is(err, errNotExist) || errors.Is(err, errBadModule) || errors.Is(err, errBadVersion) {
			return -1, os.ErrNotExist
		}
		return -1, fmt.Errorf("preparing build: %w", err)
	}

	eventc := make(chan buildUpdate, 100)
	registerBuild(logger(ctx), bs, nil, eventc, remoteIP(r))
	defer unregisterBuild(bs, eventc)

	for {
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case update := <-eventc:
			if !update.done {
				continue
			}
			if update.err != nil {
				// If the build simply can't succeed, ensure we don't log it as lookup error.
				if reason := cannotBuild(update.errOutput); reason != "unknown" {
					logger(ctx).Info("build failed, but output indicates it does not exist", "err", update.err, "reason", reason, "buildspec", bs)
					return -1, os.ErrNotExist
				}
				return -1, update.err
			}
			return int64(update.buildResult.TreeRecord.ID.Number()), nil
		}
	}
}

// ReadTileData reads the content of tile t.
// It is only invoked for hash tiles (t.L ≥ 0).
func (s serverOps) ReadTileData(ctx context.Context, t tlog.Tile) (results []byte, rerr error) {
	// log.Printf("server: ReadTileData %#v", t)
	defer observeOp(ctx, "readtiledata", &rerr, time.Now(), metricTlogOpsReadtiledataErrors, metricTlogOpsReadtiledataDuration)

	err := database.Read(ctx, func(tx *bstore.Tx) error {
		var err error
		results, err = tlog.ReadTileData(t, hashReader{tx})
		return err
	})
	return results, err
}
