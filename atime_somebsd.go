//go:build freebsd || netbsd || darwin

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
	st, ok := sys.(*syscall.Stat_t)
	if !ok {
		return time.Time{}, fmt.Errorf("fileinfo sys is not *syscall.Stat_t, but %T", sys)
	}
	sec, nsec := st.Atimespec.Unix()
	return time.Unix(sec, nsec), nil
}
