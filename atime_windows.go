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
	st, ok := sys.(*syscall.Win32FileAttributeData)
	if !ok {
		return time.Time{}, fmt.Errorf("fileinfo sys is not *syscall.Win32FileAttributeData, but %T", sys)
	}
	nsec := st.LastAccessTime.Nanoseconds()
	return time.Unix(0, nsec), nil
}
