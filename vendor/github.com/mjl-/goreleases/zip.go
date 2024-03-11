package goreleases

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func fetchZip(f *os.File, file File, dst string, permissions *Permissions) error {
	fi, err := os.Stat(dst)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("dst is not a directory")
	}
	_, err = os.Stat(filepath.Join(dst, "go"))
	if err == nil {
		return fmt.Errorf(`directory "go" already exists`)
	}
	// we assume it's a not-exists error. if it isn't, eg noperm, we'll probably get the same error later on, which is fine.

	dst = filepath.Clean(dst)

	b := &bytes.Buffer{}
	hr := &hashReader{f, sha256.New()}
	_, err = io.Copy(b, hr)
	if err != nil {
		return fmt.Errorf("fetching zip file: %v", err)
	}
	sum := fmt.Sprintf("%x", hr.h.Sum(nil))
	if sum != file.Sha256 {
		return fmt.Errorf("checksum mismatch, got %x, expected %s", sum, file.Sha256)
	}

	success := false
	defer func() {
		if !success {
			os.RemoveAll(filepath.Join(dst, "go"))
		}
	}()

	buf := bytes.NewReader(b.Bytes())
	b = nil
	r, err := zip.NewReader(buf, int64(buf.Len()))
	if err != nil {
		return fmt.Errorf("reading zip file: %v", err)
	}
	for _, zf := range r.File {
		name, err := dstName(dst, zf.Name)
		if err != nil {
			return err
		}

		if strings.HasSuffix(zf.Name, "/") {
			err = os.Mkdir(name, 0775)
			if err != nil {
				return err
			}
			continue
		}

		err = storeZip(zf, name, permissions)
		if err != nil {
			return fmt.Errorf("storing file: %v", err)
		}
	}

	success = true
	return nil
}

func storeZip(zf *zip.File, name string, perms *Permissions) error {
	sf, err := zf.Open()
	if err != nil {
		return fmt.Errorf("opening file in zip: %v", err)
	}
	defer sf.Close()

	df, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, zf.Mode()&0777)
	if err != nil {
		return fmt.Errorf("creating file: %v", err)
	}
	defer func() {
		if df != nil {
			df.Close()
		}
	}()

	if perms != nil {
		mode := perms.Mode & 0777
		if zf.Mode()&0100 == 0 {
			mode &= 0666
		}
		err = df.Chmod(mode)
		if err != nil {
			return fmt.Errorf("chmod: %s", err)
		}

		if perms.Uid >= 0 || perms.Gid >= 0 {
			err := os.Lchown(name, perms.Uid, perms.Gid)
			if err != nil {
				return fmt.Errorf("chown: %v", err)
			}
		}
	}

	err = os.Chtimes(name, zf.Modified, zf.Modified)
	if err != nil {
		return fmt.Errorf("chtimes: %v", err)
	}

	_, err = io.Copy(df, sf)
	if err != nil {
		return fmt.Errorf("writing file: %v", err)
	}
	err = df.Close()
	df = nil
	return err
}
