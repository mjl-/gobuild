// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sumdb

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/mod/sumdb/note"
	"golang.org/x/mod/sumdb/tlog"
)

// NewTestServer constructs a new TestServer
// that will sign its tree with the given signer key
// (see golang.org/x/mod/sumdb/note)
// and fetch new records as needed by calling gosum.
func NewTestServer(signer string, gosum func(path, vers string) ([]byte, error)) *TestServer {
	return &TestServer{signer: signer, gosum: gosum}
}

// A TestServer is an in-memory implementation of Server for testing.
type TestServer struct {
	signer string
	gosum  func(path, vers string) ([]byte, error)

	mu      sync.Mutex
	hashes  testHashes
	records [][]byte
	lookup  map[string]int64
}

// testHashes implements tlog.HashReader, reading from a slice.
type testHashes []tlog.Hash

func (h testHashes) ReadHashes(indexes []int64) ([]tlog.Hash, error) {
	var list []tlog.Hash
	for _, id := range indexes {
		list = append(list, h[id])
	}
	return list, nil
}

func (s *TestServer) Signed(ctx context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	size := int64(len(s.records))
	h, err := tlog.TreeHash(size, s.hashes)
	if err != nil {
		return nil, err
	}
	text := tlog.FormatTree(tlog.Tree{N: size, Hash: h})
	signer, err := note.NewSigner(s.signer)
	if err != nil {
		return nil, err
	}
	return note.Sign(&note.Note{Text: string(text)}, signer)
}

func (s *TestServer) ReadRecords(ctx context.Context, id, n int64) ([][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var list [][]byte
	for i := range n {
		if id+i >= int64(len(s.records)) {
			return nil, fmt.Errorf("missing records")
		}
		list = append(list, s.records[id+i])
	}
	return list, nil
}

func (s *TestServer) Lookup(ctx context.Context, key string) (int64, error) {
	s.mu.Lock()
	id, ok := s.lookup[key]
	s.mu.Unlock()
	if ok {
		return id, nil
	}

	// Look up module and compute go.sum lines.
	before, after, ok0 := strings.Cut(key, "@")
	if !ok0 {
		return 0, fmt.Errorf("invalid lookup key %q", key)
	}
	path, vers := before, after
	data, err := s.gosum(path, vers)
	if err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// We ran the fetch without the lock.
	// If another fetch happened and committed, use it instead.
	id, ok = s.lookup[key]
	if ok {
		return id, nil
	}

	// Add record.
	id = int64(len(s.records))
	s.records = append(s.records, data)
	if s.lookup == nil {
		s.lookup = make(map[string]int64)
	}
	s.lookup[key] = id
	hashes, err := tlog.StoredHashesForRecordHash(id, tlog.RecordHash([]byte(data)), s.hashes)
	if err != nil {
		panic(err)
	}
	s.hashes = append(s.hashes, hashes...)

	return id, nil
}

func (s *TestServer) ReadTileData(ctx context.Context, t tlog.Tile) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return tlog.ReadTileData(t, s.hashes)
}
