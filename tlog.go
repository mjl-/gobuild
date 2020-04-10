package main

import (
	"crypto/rand"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/mod/sumdb/note"
	"golang.org/x/mod/sumdb/tlog"
)

func genkey(args []string) {
	if len(args) != 1 {
		usage()
	}

	name := args[0]
	skey, vkey, err := note.GenerateKey(rand.Reader, name)
	if err != nil {
		log.Fatalf("generating key: %v", err)
	}

	log.Printf("Signer key: %s", skey)
	log.Printf("Verifier key: %s", vkey)
	log.Printf(`Configure the signer key in your server config file, and use the verifier key with the "get" subcommand.`)
}

// The on-disk record is 512 bytes: 2-byte big endian size, followed by n bytes content, followed by zero bytes.
const diskRecordSize = 512

func treeSize() (int64, error) {
	info, err := recordsFile.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size()%diskRecordSize != 0 {
		return 0, fmt.Errorf("inconsistent size of records file: %d is not multiple of diskRecordSize %d", info.Size(), diskRecordSize)
	}
	n := info.Size() / diskRecordSize
	return n, nil
}

var addSumMutex sync.Mutex

// Add successful build to transparency log. Set b.RecordNumber.
// Tmpdir is the directory where the build files reside, which is where the @index file needs to be created.
func addSum(tmpdir string, b *buildJSON) (rerr error) {
	if b.Sum == "" {
		return fmt.Errorf("missing sum")
	}

	addSumMutex.Lock()
	defer addSumMutex.Unlock()

	recordNumber, err := treeSize()
	if err != nil {
		return fmt.Errorf("determining hash count: %v", err)
	}

	hinfo, err := hashesFile.Stat()
	if err != nil {
		return fmt.Errorf("stat hashes file: %v", err)
	}
	hashesSize := hinfo.Size()
	recordsSize := recordNumber * diskRecordSize
	expHashes := tlog.StoredHashCount(recordNumber)
	if expHashes*tlog.HashSize != hashesSize {
		return fmt.Errorf("unexpected size of hashes file: for %d records, we should have %d hashes, for a total of %d bytes, but file is %d bytes", recordNumber, expHashes, expHashes*tlog.HashSize, hashesSize)
	}

	pl := filepath.Join(tmpdir, "@index")
	os.MkdirAll(filepath.Dir(pl), 0700)
	if err := ioutil.WriteFile(pl, []byte(fmt.Sprintf("%d", recordNumber)), 0666); err != nil {
		return fmt.Errorf("writing index file %s: %v", pl, err)
	}

	pkg := "/" + b.Dir
	fields := []string{
		b.Mod,
		b.Version,
		pkg,
		b.Goos,
		b.Goarch,
		b.Goversion,
		fmt.Sprintf("%d", b.Filesize),
		b.Sum,
	}
	for i, f := range fields {
		for _, c := range f {
			if c < ' ' {
				return fmt.Errorf("bad field %d in record: %q", i, f)
			}
		}
	}
	msg := []byte(strings.Join(fields, " ") + "\n")
	if len(msg) > diskRecordSize-2 {
		return fmt.Errorf("record too large")
	}
	log.Printf("adding record=%d at records=%d hashes=%d: %s", recordNumber, recordsSize, hashesSize, msg)
	diskMsg := make([]byte, 512)
	diskMsg[0] = uint8(len(msg) >> 8)
	diskMsg[1] = uint8(len(msg))
	copy(diskMsg[2:], msg)
	if _, err := recordsFile.Write(diskMsg); err != nil {
		return fmt.Errorf("write record: %v", err)
	}
	// Now that we wrote something, warn loudly when operations below fail.
	// todo: Run this operations in a transaction.
	defer func() {
		if rerr != nil {
			log.Printf("WARNING: failure while adding sum number %d. This means the records and hashes files are likely in inconsistent state!", recordNumber)
		}
	}()
	if err = recordsFile.Sync(); err != nil {
		return fmt.Errorf("sync records file: %v", err)
	}

	hashes, err := tlog.StoredHashes(recordNumber, msg, hashReader{})
	if err != nil {
		return fmt.Errorf("calculating hashes to store: %v", err)
	}

	hashBuf := make([]byte, len(hashes)*tlog.HashSize)
	for i, h := range hashes {
		copy(hashBuf[i*tlog.HashSize:], h[:])
	}
	if _, err := hashesFile.Write(hashBuf); err != nil {
		return fmt.Errorf("write hashes: %v", err)
	}
	if err = hashesFile.Sync(); err != nil {
		return fmt.Errorf("sync hashes file: %v", err)
	}

	b.RecordNumber = recordNumber

	return nil
}
