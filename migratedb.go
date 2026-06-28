package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/mod/sumdb/note"
	"golang.org/x/mod/sumdb/tlog"

	"github.com/mjl-/gobuild/internal/sumdb"
)

// The on-disk record is 512 bytes: 2-byte big endian size, followed by n bytes content, followed by zero bytes.
const diskRecordSize = 512

// Before the bstore database, we had this file layout (all gone now):
//
// $datadir/buildfailures.txt
// $datadir/result/binary-cache-size.txt
// $datadir/result/<sum-first-letter>/<sum>/{log.gz,recordnumber,builderror.txt,binary.gz}, with recordnumber only for successes, binary.gz only when not yet cleared by cache, and builderror.txt optional.
// $datadir/sum/hashes, all hashes of the tree
// $datadir/sum/records, all records of the tree, each a 512-byte sector starting, starting with a 2-byte size, uint16 big endian.
//
// The migration inserts all hashes & records, along with the logs, and moving
// binaries that exist. Afterwards the remaining/original files are moved to
// $datadir/migrated. Failed builds are not inserted as result.
func migrateToDB(ctx context.Context) (rerr error) {
	log := logger(ctx)

	mustOpenDB(ctx)

	defer func() {
		if rerr != nil {
			dbpath := filepath.Join(config.DataDir, "gobuild.db")
			if err := os.Remove(dbpath); err != nil && !errors.Is(err, fs.ErrNotExist) {
				log.Error("cleaning up $datadir/gobuild.db", "err", err, "path", dbpath)
			}
		}
	}()

	migrated := filepath.Join(config.DataDir, "migrated")
	if err := os.MkdirAll(migrated, 0777); err != nil {
		return fmt.Errorf("making $datadir/migrated for remaining files: %v", err)
	}

	// We rename binary files at the end, after committing the database.
	type rename struct {
		Old, New string
	}
	var renames []rename

	log.Info("building database from hashes and records files")

	// Open hashes file.
	hashesPath := filepath.Join(config.DataDir, "sum", "hashes")
	hf, err := os.Open(hashesPath)
	if err != nil {
		return fmt.Errorf("open $datadir/sum/hashes: %v", err)
	}
	defer hf.Close()

	// Open records file.
	recordsPath := filepath.Join(config.DataDir, "sum", "records")
	rf, err := os.Open(recordsPath)
	if err != nil {
		return fmt.Errorf("open $datadir/sum/records file: %v", err)
	}
	defer rf.Close()

	// Verify state is sane before starting migration.
	if _, err := verifySumStateMigrate(ctx, hf, rf); err != nil {
		return fmt.Errorf("verifying hashes & records before starting: %v", err)
	}

	// We keep chunks of work and commit. Bolt keeps changes in memory. Large
	// migrations would require a lot of working memory. By committing every 10k hashes
	// and every 1k records, we should prevent swapping in most cases.
	tx, err := database.Begin(ctx, true)
	if err != nil {
		return fmt.Errorf("begin transaction: %v", err)
	}
	defer func() {
		if tx != nil {
			err := tx.Rollback()
			logCheck(ctx, err, "rolling back transaction")
		}
	}()

	// Insert all hashes into database.
	br := bufio.NewReader(hf)
	var hashCount int64
	for {
		var h tlog.Hash
		if _, err := io.ReadFull(br, h[:]); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("read hash: %v", err)
		}
		th := TreeHash{Number(hashCount).ID(), h}
		if err := tx.Insert(&th); err != nil {
			return fmt.Errorf("insert hash: %v", err)
		}
		hashCount++
		if hashCount%10000 == 0 {
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("committing: %v", err)
			}
			tx, err = database.Begin(ctx, true)
			if err != nil {
				return fmt.Errorf("begin transaction: %v", err)
			}
			log.Info("committed 10k hashes")
		}
	}

	// Insert all records into database.
	br = bufio.NewReader(rf)
	var recordCount int64

	processRecord := func(record *Record) error {
		result := record.result()

		dir := storeDir(record.buildSpec)

		// Verify recordnumber has the expected value.
		f, err := os.Open(filepath.Join(dir, "recordnumber"))
		if err != nil {
			return fmt.Errorf("open recordnumber: %v", err)
		}
		defer f.Close()
		if st, err := f.Stat(); err != nil {
			return fmt.Errorf("stat recordnumber: %v", err)
		} else {
			result.Created = st.ModTime()
		}
		rnbuf, err := os.ReadFile(filepath.Join(dir, "recordnumber"))
		if err != nil {
			return fmt.Errorf("read recordnumber: %v", err)
		}
		if num, err := strconv.ParseInt(string(rnbuf), 10, 64); err != nil {
			return fmt.Errorf("parse recordnumber: %v", err)
		} else if num != recordCount {
			return fmt.Errorf("unexpected recordnumber, got %d, expected %d", num, recordCount)
		} else {
			result.TreeRecordID = Number(num).ID()
		}
		// no ErrorReason

		// Prepare moving binary.
		p := filepath.Join(dir, "binary.gz")
		if fi, err := os.Stat(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("stat binary.gz: %v", err)
		} else if err == nil {
			result.FileSizeGz = fi.Size()
			renames = append(renames, rename{p, filepath.Join(config.DataDir, "binaries", fmt.Sprintf("%d.gz", result.TreeRecordID))})
		}

		if err := tx.Insert(&result); err != nil {
			return fmt.Errorf("inserting result: %v", err)
		}

		// Read the build log and insert it.
		var logBuf []byte // gzipped, absent if nil
		lf, err := os.Open(filepath.Join(dir, "log.gz"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("open log.gz for record: %v", err)
		} else if err == nil {
			defer lf.Close()
			if logBuf, err = io.ReadAll(lf); err != nil {
				return fmt.Errorf("read log.gz for record: %v", err)
			}

			bl := BuildLog{ID: result.ID, Data: logBuf}
			if err := tx.Insert(&bl); err != nil {
				return fmt.Errorf("insert build log: %v", err)
			}
		}

		// Insert the record.
		rec := TreeRecord{Number(recordCount).ID(), record.Filesize, record.Sum, result.ID}
		if err := tx.Insert(&rec); err != nil {
			return fmt.Errorf("inserting record: %v", err)
		}
		recordCount++

		if recordCount%1000 == 0 {
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("committing transaction: %v", err)
			}
			tx, err = database.Begin(ctx, true)
			if err != nil {
				return fmt.Errorf("begin transaction: %v", err)
			}
			log.Info("committed 1k records")
		}

		return nil
	}

	var recordBuf [512]byte
	for {
		if _, err := io.ReadFull(br, recordBuf[:]); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("read size of record: %v", err)
		}
		size := int(recordBuf[0])<<8 | int(recordBuf[1])
		record, err := parseRecord(recordBuf[2 : 2+size])
		if err != nil {
			return fmt.Errorf("parse record: %v", err)
		}

		if err := processRecord(record); err != nil {
			return fmt.Errorf("inserting record number %d: %v", recordCount, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %v", err)
	}
	tx = nil

	// Rename files.
	log.Info("renaming gzipped binaries")
	for _, ren := range renames {
		if err := os.Rename(ren.Old, ren.New); err != nil {
			log.Error("rename after migration, continuing", "err", err, "old", ren.Old, "new", ren.New)
		}
	}

	// Remove old remaining files.
	log.Info("moving old/remaining files", "dest", migrated)
	move := func(p string) {
		dest := filepath.Join(migrated, filepath.Base(p))
		if err := os.Rename(p, dest); err != nil {
			log.Error("moving after migration", "err", err, "path", p, "dest", dest)
		}
	}
	move(filepath.Join(config.DataDir, "buildfailures.txt"))
	move(filepath.Join(config.DataDir, "result"))
	move(filepath.Join(config.DataDir, "sum"))

	return nil
}

func verifySumStateMigrate(ctx context.Context, hashesFile, recordsFile *os.File) (int64, error) {
	// Verify records & hashes files have consistent sizes.
	numRecords, err := fileTreeSize(recordsFile)
	if err != nil {
		return -1, fmt.Errorf("finding number of records in tlog: %v", err)
	}
	if info, err := hashesFile.Stat(); err != nil {
		return -1, fmt.Errorf("stat on hashes file: %v", err)
	} else if hashCount := tlog.StoredHashCount(numRecords); hashCount*tlog.HashSize != info.Size() {
		return -1, fmt.Errorf("inconsistent size of hashes file of %d bytes for %d records, should be %d", info.Size(), numRecords, hashCount*tlog.HashSize)
	}

	// For the latest record on disk, verify the hashes on disk match the record.
	if numRecords == 0 {
		return 0, nil
	}

	lastRecordNum := numRecords - 1
	records, err := fileServerOps{hashesFile: hashesFile, recordsFile: recordsFile}.ReadRecords(ctx, lastRecordNum, 1)
	if err != nil {
		return -1, fmt.Errorf("reading last record: %v", err)
	}
	hashes, err := tlog.StoredHashes(lastRecordNum, records[0], fileHashReader{hashesFile})
	if err != nil {
		return -1, fmt.Errorf("calculating hashes for most recent record: %v", err)
	}
	buf := make([]byte, len(hashes)*tlog.HashSize)
	if _, err := hashesFile.ReadAt(buf, tlog.StoredHashIndex(0, lastRecordNum)*tlog.HashSize); err != nil {
		return -1, fmt.Errorf("reading hashes for verification: %v", err)
	}
	for i := range hashes {
		o := i * tlog.HashSize
		h := buf[o : o+tlog.HashSize]
		if !bytes.Equal(hashes[i][:], h) {
			return -1, fmt.Errorf("hash %d mismatch for last record %d, got %x, expect %x", i, lastRecordNum, h, hashes[i][:])
		}
	}

	// Also check if the recordnumber file is available, i.e. if a lookup will succeed.
	record, err := parseRecord(records[0])
	if err != nil {
		return -1, fmt.Errorf("parsing last record: %v", err)
	}
	if buf, err := os.ReadFile(filepath.Join(storeDir(record.buildSpec), "recordnumber")); err != nil {
		return -1, fmt.Errorf("open recordnumber: %v", err)
	} else if num, err := strconv.ParseInt(string(buf), 10, 64); err != nil {
		return -1, fmt.Errorf("parse recordnumber from file: %v", err)
	} else if num != lastRecordNum {
		return -1, fmt.Errorf("inconsistent last recordnumber %d, expected %d", num, lastRecordNum)
	}

	// And check if the hash of the binary matches the sum.
	h := sha256.New()
	f, err := os.Open(filepath.Join(storeDir(record.buildSpec), "binary.gz"))
	if err != nil && errors.Is(err, fs.ErrNotExist) {
		return numRecords, nil
	} else if err != nil {
		return -1, fmt.Errorf("open binary.gz for verification: %v", err)
	}
	defer f.Close()
	if gzr, err := gzip.NewReader(f); err != nil {
		return -1, fmt.Errorf("gzip reader for binary.gz: %v", err)
	} else if _, err := io.Copy(h, gzr); err != nil {
		return -1, fmt.Errorf("reading binary.gz for verification: %v", err)
	} else if sum := (buildSum{[20]byte(h.Sum(nil)[:20])}); sum != record.Sum {
		return -1, fmt.Errorf("latest binary.gz sum mismatch, got %s, expect %s", sum, record.Sum)
	} else if err := f.Close(); err != nil {
		return -1, fmt.Errorf("close binary.gz: %v", err)
	}
	return numRecords, nil
}

func fileTreeSize(recordsFile *os.File) (int64, error) {
	if info, err := recordsFile.Stat(); err != nil {
		return 0, err
	} else if info.Size()%diskRecordSize != 0 {
		return 0, fmt.Errorf("inconsistent size of records file: %d is not multiple of diskRecordSize %d", info.Size(), diskRecordSize)
	} else {
		n := info.Size() / diskRecordSize
		return n, nil
	}
}

type fileServerOps struct {
	hashesFile, recordsFile *os.File
	signer                  note.Signer
}

var _ sumdb.ServerOps = fileServerOps{}

type fileHashReader struct {
	hashesFile *os.File
}

func (h fileHashReader) ReadHashes(indexes []int64) ([]tlog.Hash, error) {
	hashes := make([]tlog.Hash, len(indexes))

	for i, index := range indexes {
		if _, err := h.hashesFile.ReadAt(hashes[i][:], index*tlog.HashSize); err != nil {
			return nil, err
		}
	}
	return hashes, nil
}

// Signed returns the signed hash of the latest tree.
func (s fileServerOps) Signed(ctx context.Context) (result []byte, rerr error) {
	if n, err := fileTreeSize(s.recordsFile); err != nil {
		return nil, err
	} else if h, err := tlog.TreeHash(n, fileHashReader{s.hashesFile}); err != nil {
		return nil, err
	} else {
		text := tlog.FormatTree(tlog.Tree{N: n, Hash: h})
		return note.Sign(&note.Note{Text: string(text)}, s.signer)
	}
}

// ReadRecords returns the content for the n records num through num+n-1.
func (s fileServerOps) ReadRecords(ctx context.Context, num, n int64) (results [][]byte, rerr error) {
	if n <= 0 {
		return nil, fmt.Errorf("bad n")
	}

	all := make([]byte, n*diskRecordSize)
	if _, err := s.recordsFile.ReadAt(all, num*diskRecordSize); err != nil {
		return nil, fmt.Errorf("reading records: %v", err)
	}
	result := make([][]byte, n)
	for i := range n {
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
func (s fileServerOps) Lookup(ctx context.Context, key string) (results int64, rerr error) {
	// log.Printf("server: Lookup %q", key)
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

	p := filepath.Join(storeDir(bs), "recordnumber")
	if buf, err := os.ReadFile(p); err != nil {
		return -1, err
	} else {
		return strconv.ParseInt(string(buf), 10, 64)
	}
}

// ReadTileData reads the content of tile t.
// It is only invoked for hash tiles (t.L ≥ 0).
func (s fileServerOps) ReadTileData(ctx context.Context, t tlog.Tile) (results []byte, rerr error) {
	return tlog.ReadTileData(t, fileHashReader{s.hashesFile})
}

// Local directory where results are stored, both successful and failed.
// Directories of failed builds can be removed, for a retry.
func storeDir(bs buildSpec) string {
	sha := sha256.Sum256([]byte(bs.String()))
	sum := base64.RawURLEncoding.EncodeToString(sha[:20])
	return filepath.Join(config.DataDir, "result", sum[:1], sum)
}
