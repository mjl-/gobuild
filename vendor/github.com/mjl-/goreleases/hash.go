package goreleases

import (
	"hash"
	"io"
)

type hashReader struct {
	r io.Reader
	h hash.Hash
}

func (hr *hashReader) Read(buf []byte) (n int, err error) {
	n, err = hr.r.Read(buf)
	if n > 0 {
		hr.h.Write(buf[:n])
	}
	return
}
