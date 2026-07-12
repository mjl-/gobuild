package main

import (
	"fmt"
	"log/slog"
	"os"
)

func writeFileSync(p string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE, perm)
	if err != nil {
		return fmt.Errorf("create %s: %v", p, err)
	}
	defer func() {
		if f == nil {
			return
		}
		if err := f.Close(); err != nil {
			slog.Error("closing cache file after failure", "err", err, "path", p)
		}
		if err := os.Remove(p); err != nil {
			slog.Error("removing cache file after failure", "err", err, "path", p)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %s: %v", p, err)
	} else if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %v", p, err)
	} else {
		err := f.Close()
		f = nil
		if err != nil {
			return fmt.Errorf("close %s: %v", p, err)
		}
	}
	return nil
}
