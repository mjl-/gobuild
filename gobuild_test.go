package main

import (
	"testing"
)

func TestRegexpSourceError(t *testing.T) {
	if !reSourceError.MatchString("go/pkg/mod/golang.org/x/sys@v0.0.0-20190507160741-ecd444e8653b/unix/syscall_unix_gc.go:12:6: missing function body") {
		t.Fatalf("expected match")
	}
}
