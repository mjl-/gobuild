package main

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

func atime(fi os.FileInfo) (time.Time, error) {
	sys := fi.Sys()
	if sys == nil {
		return time.Time{}, fmt.Errorf("fileinfo sys is nil")
	}
	st, ok := sys.(*syscall.Dir)
	if !ok {
		return time.Time{}, fmt.Errorf("fileinfo sys is not *syscall.Dir, but %T", sys)
	}
	sec := st.Atime
	return time.Unix(int64(sec), 0), nil
}
