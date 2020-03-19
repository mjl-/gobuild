package goreleases

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
)

// Permissions to set on extract files and directories, overriding permissions in the archive.
type Permissions struct {
	Uid  int
	Gid  int
	Mode os.FileMode // Mode to use for extract files and directories. Files are masked with 0777 or 0666 depending on whether 0100 is set.
}

// Fetch downloads, extracts and verifies a Go release represented by file into directory dst.
// After a successful fetch, dst contains a directory "go" with the specified release.
// Directory dst must exist. It must not already contain a "go" subdirectory.
// Only files with filenames ending .tar.gz are supported. So no macOS or Windows.
// If permissions is not nil, it is applied to extracted files and directories.
func Fetch(file File, dst string, permissions *Permissions) error {
	if !strings.HasSuffix(file.Filename, ".tar.gz") {
		return fmt.Errorf("file extension not supported, only .tar.gz supported")
	}

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
	_, err = os.Stat(path.Join(dst, "go"))
	if err == nil {
		return fmt.Errorf(`directory "go" already exists`)
	}
	// we assume it's a not-exists error. if it isn't, eg noperm, we'll probably get the same error later on, which is fine.

	dst = path.Clean(dst) + "/"

	url := "https://golang.org/dl/" + file.Filename
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
			os.RemoveAll(path.Join(dst, "go"))
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

		err = store(dst, tr, h, name, permissions)
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

func dstName(dst, name string) (string, error) {
	if name != "go" && !strings.HasPrefix(name, "go/") {
		return "", fmt.Errorf("path %q: does not start with \"go\"", name)
	}

	r := path.Clean(path.Join(dst, name))
	if !strings.HasPrefix(r, dst) {
		return "", fmt.Errorf("bad path %q in archive, resulting in path %q outside dst %q", name, r, dst)
	}
	return r, nil
}

func store(dst string, tr *tar.Reader, h *tar.Header, name string, perms *Permissions) error {
	os.MkdirAll(path.Dir(name), 0777)

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

			err := os.Lchown(name, perms.Uid, perms.Gid)
			if err != nil {
				return fmt.Errorf("chown: %v", err)
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
