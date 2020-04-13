package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/mod/sumdb/tlog"
)

// The on-disk record is 512 bytes: 2-byte big endian size, followed by n bytes content, followed by zero bytes.
const diskRecordSize = 512

func treeSize() (int64, error) {
	if info, err := recordsFile.Stat(); err != nil {
		return 0, err
	} else if info.Size()%diskRecordSize != 0 {
		return 0, fmt.Errorf("inconsistent size of records file: %d is not multiple of diskRecordSize %d", info.Size(), diskRecordSize)
	} else {
		n := info.Size() / diskRecordSize
		return n, nil
	}
}

var addSumMutex sync.Mutex

// Add (successful) build result to transparency log. Returns record number.
// Tmpdir is the directory where the build files reside, where addSum writes the
// "recordnumber" file. This directory is renamed to its final directory in
// resultDir as last step in addSum.
//
// With the current approach, we need to store to multiple locations: records,
// hashes, and the result directory. We cannot make the changes atomically. If an
// error happens halfway through, we are in an inconsistent state. We do log an
// error in that case, and will detect it and fail on startup.
func addSum(tmpdir string, br buildResult) (rnum int64, rerr error) {
	defer func() {
		if rerr != nil {
			metricTlogAddErrors.Inc()
		}
	}()

	if br.Sum == "" {
		return -1, fmt.Errorf("missing sum")
	}

	// Only one addSum at a time. And this is used for graceful shutdown on signals.
	addSumMutex.Lock()
	defer addSumMutex.Unlock()

	storeDir := br.storeDir()
	if _, err := os.Stat(storeDir); err == nil {
		return -1, fmt.Errorf("store dir for build result already exists")
	} else if !os.IsNotExist(err) {
		return -1, fmt.Errorf("stat on store dir for build result: %v", err)
	}

	// Find the next/new record number we'll be adding.
	recordNumber, err := treeSize()
	if err != nil {
		return -1, fmt.Errorf("determining hash count: %v", err)
	}

	// Verify consistency of records & hashes files.
	hinfo, err := hashesFile.Stat()
	if err != nil {
		return -1, fmt.Errorf("stat hashes file: %v", err)
	}
	hashesSize := hinfo.Size()
	recordsSize := recordNumber * diskRecordSize
	expHashes := tlog.StoredHashCount(recordNumber)
	if expHashes*tlog.HashSize != hashesSize {
		metricTlogConsistencyErrors.Inc()
		return -1, fmt.Errorf("unexpected size of hashes file: for %d records, we should have %d hashes, for a total of %d bytes, but file is %d bytes", recordNumber, expHashes, expHashes*tlog.HashSize, hashesSize)
	}

	// Write the recordnumber file to the tmpdir. A lookup reads this file to find the
	// index in the records file to read the record at. We move this dir into place as
	// last step, from then on lookups will succeed.
	pl := filepath.Join(tmpdir, "recordnumber")
	if err := ioutil.WriteFile(pl, []byte(fmt.Sprintf("%d", recordNumber)), 0666); err != nil {
		return -1, fmt.Errorf("writing index file %s: %v", pl, err)
	}

	// Pack record and check validity, but don't write yet.
	msg, err := br.packRecord()
	if err != nil {
		return -1, err
	}
	if len(msg) > diskRecordSize-2 {
		return -1, fmt.Errorf("record too large")
	}

	// Calculate the hashes we need to write for the new record.
	hashes, err := tlog.StoredHashes(recordNumber, msg, hashReader{})
	if err != nil {
		return -1, fmt.Errorf("calculating hashes to store: %v", err)
	}

	// We know we are doing this, so log what we are going to write where.
	if _, err := fmt.Fprintf(sumLogFile, "adding record=%d at records=%d hashes=%d: %s", recordNumber, recordsSize, hashesSize, msg); err != nil {
		return -1, fmt.Errorf("writing sum log: %v", err)
	}

	// Combine the hashes into a single buffer so we can do one big write. This is our
	// first permanent write. It's more likely to complete succeed or fail with a
	// single write, and a single write is faster.
	hashBuf := make([]byte, len(hashes)*tlog.HashSize)
	for i, h := range hashes {
		copy(hashBuf[i*tlog.HashSize:], h[:])
	}
	if _, err := hashesFile.Write(hashBuf); err != nil {
		return -1, fmt.Errorf("write hashes: %v", err)
	}
	// Now that we wrote something permanently, warn loudly when operations below fail.
	// todo: Use better storage mechanism, with transactions.
	defer func() {
		if rerr != nil {
			metricTlogConsistencyErrors.Inc()
			log.Printf("CRITICAL: Failure while adding record number %d, key %s, storeDir %s. This means the records and hashes files and result dir are likely in inconsistent state! Error: %s", recordNumber, br.String(), br.storeDir(), rerr)
		}
	}()
	if err := hashesFile.Sync(); err != nil {
		return -1, fmt.Errorf("sync hashes file: %v", err)
	}

	// Write the record.
	diskMsg := make([]byte, 512)
	diskMsg[0] = uint8(len(msg) >> 8)
	diskMsg[1] = uint8(len(msg))
	copy(diskMsg[2:], msg)
	if _, err := recordsFile.Write(diskMsg); err != nil {
		return -1, fmt.Errorf("write record: %v", err)
	} else if err := recordsFile.Sync(); err != nil {
		return -1, fmt.Errorf("sync records file: %v", err)
	}

	// Put the tmp directory in place. From now on, lookups will succeed.
	if err := os.Rename(tmpdir, storeDir); err != nil {
		return -1, fmt.Errorf("renaming to final directory in resultDir: %w", err)
	}

	metricTlogRecords.Inc()

	return recordNumber, nil
}
