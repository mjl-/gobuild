package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/mod/sumdb/tlog"

	"github.com/mjl-/bstore"
)

var addSumMutex sync.Mutex

// Add a successful build result to transparency log.
// File tmpBinaryPath is moved to the $datadir/binaries with $recordid.gz as
// filename.
func addSum(ctx context.Context, record Record, buildLogGz []byte, tmpBinaryPath string, filesizeGz int64) (br BuildResult, rerr error) {
	defer func() {
		if rerr != nil {
			metricTlogAddErrors.Inc()
		}
	}()

	// Only one addSum at a time. And this is used for graceful shutdown on signals.
	addSumMutex.Lock()
	defer addSumMutex.Unlock()

	err := database.Write(ctx, func(tx *bstore.Tx) error {
		// todo: don't use count?
		recordCount, err := bstore.QueryTx[TreeRecord](tx).Count()
		if err != nil {
			return fmt.Errorf("get tree record count: %w", err)
		}
		recordID := ID(recordCount + 1)

		hashCount, err := bstore.QueryTx[TreeHash](tx).Count()
		if err != nil {
			return fmt.Errorf("get tree hash count: %w", err)
		}

		// Verify consistency of records & hashes files.
		expHashCount := tlog.StoredHashCount(int64(recordCount))
		if expHashCount != int64(hashCount) {
			metricTlogConsistencyErrors.Inc()
			return fmt.Errorf("unexpected hash count in database: for %d records, we should have %d hashes, but we have %d hashes", recordCount, expHashCount, hashCount)
		}

		result := record.result()
		result.FileSizeGz = filesizeGz
		result.TreeRecordID = recordID
		if err := tx.Insert(&result); err != nil {
			return fmt.Errorf("inserting build result in database: %w", err)
		}

		buildLog := BuildLog{ID: result.ID, Data: buildLogGz}
		if err := tx.Insert(&buildLog); err != nil {
			return fmt.Errorf("inserting build log in database: %w", err)
		}

		record := TreeRecord{recordID, record.Filesize, record.Sum, result.ID}
		if err := tx.Insert(&record); err != nil {
			return fmt.Errorf("inserting record in database: %w", err)
		}

		br = BuildResult{result, record}

		// Pack record and check validity.
		msg, err := br.Record().Pack()
		if err != nil {
			return fmt.Errorf("pack record: %w", err)
		}

		// Calculate the hashes we need to write for the new record.
		hashes, err := tlog.StoredHashes(int64(record.ID.Number()), msg, hashReader{tx})
		if err != nil {
			return fmt.Errorf("calculating hashes to store: %v", err)
		}

		// Insert new hashes.
		for _, h := range hashes {
			th := TreeHash{ID: ID(hashCount + 1), Sum: h}
			if err := tx.Insert(&th); err != nil {
				return fmt.Errorf("insert tree hash: %w", err)
			}
			hashCount++
		}

		// Log what we are writing.
		if sumLogFile != nil {
			if _, err := fmt.Fprintf(sumLogFile, "adding recordnum=%d resultid=%d buildspec=%s sum=%s record=%s hashesindex=%d hashes=%v", record.ID.Number(), result.ID, result.buildSpec(), record.Sum, msg, hashCount, hashes); err != nil {
				return fmt.Errorf("writing sum log: %v", err)
			}
		}

		logger(ctx).Info("adding record", "recordnumber", record.ID.Number(), "resultid", result.ID, "buildspec", result.buildSpec(), "sum", record.Sum, "record", msg, "hashesindex", hashCount, "hashes", hashes)

		return nil
	})
	if err != nil {
		return BuildResult{}, fmt.Errorf("adding record to transparency log: %w", err)
	}

	binaryPath := filepath.Join(config.DataDir, "binaries", fmt.Sprintf("%d.gz", br.TreeRecord.ID))
	if err := os.Rename(tmpBinaryPath, binaryPath); err != nil {
		logger(ctx).Error("moving binary path for newly added record", "err", err, "tmppath", tmpBinaryPath, "destpath", binaryPath)
	}

	metricTlogRecords.Inc()
	return br, nil
}
