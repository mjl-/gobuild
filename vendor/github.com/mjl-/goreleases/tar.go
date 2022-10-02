package goreleases

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

func fetchTgz(file File, dst string, permissions *Permissions) error {
	fi, err := os.Stat(dst)
	if err != nil && os.IsNotExist(err) {
		return fmt.Errorf("dst does not exist")
	}
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

	url := "https://go.dev/dl/" + file.Filename
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("downloading file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("downloading file: status %d: %s", resp.StatusCode, resp.Status)
	}

	hr := &hashReader{resp.Body, sha256.New()}

	gzr, err := gzip.NewReader(hr)
	if err != nil {
		return fmt.Errorf("gzip reader: %s", err)
	}
	defer gzr.Close()

	success := false
	defer func() {
		if !success {
			os.RemoveAll(filepath.Join(dst, "go"))
		}
	}()

	tr := tar.NewReader(gzr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading next header from tar file: %s", err)
		}

		name, err := dstName(dst, h.Name)
		if err != nil {
			return err
		}

		err = storeTar(dst, tr, h, name, permissions)
		if err != nil {
			return err
		}
	}

	sum := fmt.Sprintf("%x", hr.h.Sum(nil))
	if sum != file.Sha256 {
		return fmt.Errorf("checksum mismatch, got %x, expected %s", sum, file.Sha256)
	}
	success = true
	return nil
}

func storeTar(dst string, tr *tar.Reader, h *tar.Header, name string, perms *Permissions) error {
	os.MkdirAll(filepath.Dir(name), 0777)

	switch h.Typeflag {
	case tar.TypeReg:
		f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, os.FileMode(h.Mode)&0777)
		if err != nil {
			return err
		}
		defer func() {
			if f != nil {
				f.Close()
			}
		}()
		lr := io.LimitReader(tr, h.Size)
		n, err := io.Copy(f, lr)
		if err != nil {
			return fmt.Errorf("extracting: %v", err)
		}
		if n != h.Size {
			return fmt.Errorf("extracting %d bytes, expected %d", n, h.Size)
		}
		if perms != nil {
			mode := perms.Mode & 0777
			if h.Mode&0100 == 0 {
				mode &= 0666
			}
			err = f.Chmod(mode)
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
		err = os.Chtimes(name, h.AccessTime, h.ModTime)
		if err != nil {
			return fmt.Errorf("chtimes: %v", err)
		}
		err = f.Close()
		if err != nil {
			return fmt.Errorf("close: %s", err)
		}
		f = nil
		return nil
	case tar.TypeLink:
		linkname, err := dstName(dst, h.Linkname)
		if err != nil {
			return err
		}
		return os.Link(linkname, name)
	case tar.TypeSymlink:
		linkname, err := dstName(dst, h.Linkname)
		if err != nil {
			return err
		}
		err = os.Symlink(linkname, name)
		if err != nil {
			return err
		}
		if perms != nil {
			err := os.Lchown(name, perms.Uid, perms.Gid)
			if err != nil {
				return fmt.Errorf("chown: %v", err)
			}
		}
		return nil
	case tar.TypeDir:
		err := os.Mkdir(name, 0777)
		if err != nil {
			return fmt.Errorf("mkdir: %v", err)
		}
		if perms != nil {
			err = os.Chmod(name, perms.Mode)
			if err != nil {
				return fmt.Errorf("chmod: %s", err)
			}

			err := os.Lchown(name, perms.Uid, perms.Gid)
			if err != nil {
				return fmt.Errorf("chown: %v", err)
			}
		}
		err = os.Chtimes(name, h.AccessTime, h.ModTime)
		if err != nil {
			return fmt.Errorf("chtimes: %v", err)
		}
		return nil
	case tar.TypeXGlobalHeader, tar.TypeGNUSparse:
		return nil
	}
	return fmt.Errorf("unsupported tar header typeflag %v", h.Typeflag)
}
