package main

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mjl-/gobuild/internal/sumdb"

	"golang.org/x/mod/sumdb/note"
	"golang.org/x/mod/sumdb/tlog"
)

type server struct {
	signer note.Signer
}

var _ sumdb.ServerOps = server{}

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

// Signed returns the signed hash of the latest tree.
func (s server) Signed(ctx context.Context) ([]byte, error) {
	n, err := treeSize()
	if err != nil {
		return nil, err
	}

	h, err := tlog.TreeHash(n, hashReader{})
	if err != nil {
		return nil, err
	}
	text := tlog.FormatTree(tlog.Tree{N: n, Hash: h})
	return note.Sign(&note.Note{Text: string(text)}, s.signer)
}

// ReadRecords returns the content for the n records id through id+n-1.
func (s server) ReadRecords(ctx context.Context, id, n int64) ([][]byte, error) {
	// log.Printf("server: ReadRecords %d %d", id, n)

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
func (s server) Lookup(ctx context.Context, key string) (int64, error) {
	// log.Printf("server: Lookup %q", key)

	k, err := parseLookupKey(key)
	if err != nil {
		return -1, os.ErrNotExist
	}

	if !targets.valid(k.Goos + "/" + k.Goarch) {
		return -1, os.ErrNotExist
	}

	p := filepath.Join(config.DataDir, "result", key, "@index")
	buf, err := ioutil.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return lookupBuild(ctx, key, k)
		}
		return -1, err
	}
	return strconv.ParseInt(string(buf), 10, 64)
}

// Returns os.ErrNotExist (not wrapped) if the fault is in the request, to cause a http 404 response for the lookup.
func lookupBuild(ctx context.Context, key string, k lookupKey) (int64, error) {
	lpath := filepath.Join(config.DataDir, "result", key)

	// Before attempting to build, check we don't have a failed build already.
	if _, err := os.Stat(filepath.Join(lpath, "log.gz")); err == nil {
		return -1, os.ErrNotExist
	}

	req := request{
		k.Mod,
		k.Version,
		k.Dir,
		k.Goos,
		k.Goarch,
		k.Goversion,
		pageIndex,
		"",
	}

	// Attempt to build.
	if err := prepareBuild(req); err != nil {
		if errors.Is(err, errBadGoversion) || errors.Is(err, os.ErrNotExist) || errors.Is(err, errNotExist) || errors.Is(err, errBadModule) || errors.Is(err, errBadVersion) {
			return -1, os.ErrNotExist
		}
		return -1, fmt.Errorf("preparing build: %w", err)
	}

	eventc := make(chan buildUpdate, 100)
	registerBuild(req, eventc)

	for {
		select {
		case <-ctx.Done():
			unregisterBuild(req, eventc)
			return 0, ctx.Err()
		case update := <-eventc:
			if !update.done {
				continue
			}
			unregisterBuild(req, eventc)
			if update.err != nil {
				// todo: turn some errors into "file not found".
				return -1, fmt.Errorf("build failed: %s", update.err)
			}
			return update.result.RecordNumber, nil
		}
	}
}

type lookupKey struct {
	Goos, Goarch, Goversion, Mod, Version, Dir string
}

func parseLookupKey(s string) (k lookupKey, err error) {
	t := strings.SplitN(s, "/", 2)
	if len(t) != 2 {
		err = fmt.Errorf("missing slash")
		return
	}
	s = t[1]
	t = strings.Split(t[0], "-")
	if len(t) != 3 {
		err = fmt.Errorf("bad goos-goarch-goversion")
		return
	}
	k.Goos = t[0]
	k.Goarch = t[1]
	k.Goversion = t[2]
	t = strings.SplitN(s, "@", 2)
	if len(t) != 2 {
		err = fmt.Errorf("missing @ version")
		return
	}
	k.Mod = t[0]
	s = t[1]
	t = strings.SplitN(s, "/", 2)
	if len(t) != 2 {
		err = fmt.Errorf("missing slash for package dir")
		return
	}
	k.Version = t[0]
	s = t[1]
	if s != "" && !strings.HasSuffix(s, "/") {
		err = fmt.Errorf("missing slash at end of package dir")
		return
	}
	k.Dir = s
	if path.Clean(k.Mod) != k.Mod {
		err = fmt.Errorf("non-canonical module name")
		return
	}
	if k.Dir != "" && path.Clean(k.Dir)+"/" != k.Dir {
		err = fmt.Errorf("non-canonical package dir")
		return
	}
	return
}

// ReadTileData reads the content of tile t.
// It is only invoked for hash tiles (t.L ≥ 0).
func (s server) ReadTileData(ctx context.Context, t tlog.Tile) ([]byte, error) {
	// log.Printf("server: ReadTileData %#v", t)

	return tlog.ReadTileData(t, hashReader{})
}
