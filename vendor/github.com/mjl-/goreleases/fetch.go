package goreleases

import (
	"io"
	"bytes"
	"net/http"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/openpgp"
)

// Permissions to set on extract files and directories, overriding permissions in the archive.
// Uid and gid are only set when at least one of them is >= 0. Setting uid/gid
// will fail on Windows.
type Permissions struct {
	Uid  int
	Gid  int
	Mode os.FileMode // Mode to use for extract files and directories. Files are masked with 0777 or 0666 depending on whether 0100 is set.
}

// Fetch downloads a toolchain represented, downloads and verifies its gpg
// signature, and extracts it into directory dst.
//
// After a successful fetch, dst contains a directory "go" with the specified release.
// Directory dst must exist. It must not already contain a "go" subdirectory.
//
// Only files with filenames ending .tar.gz and .zip can be fetched. Tar.gz
// files are extracted while fetched. Zip files are first read into memory,
// then extracted.
//
// If permissions is not nil, it is applied to extracted files and directories.
func Fetch(file File, dst string, permissions *Permissions) error {
	// Fetch .asc file with signature.
	resp, err := http.Get("https://go.dev/dl/" + file.Filename + ".asc")
	if err != nil {
		return fmt.Errorf("getting .asc signature file: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching .asc signature file, status %v, expected 200 OK", resp.Status)
	}
	sigbuf, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read .asci signature file: %v", err)
	}

	// Temporary file to write release tgz/zip into.
	f, err := os.CreateTemp("", "goreleases-download")
	if err != nil {
		return err
	}
	defer func() {
		// We only remove once we're done. Removing files that are in use doesn't work well
		// with Windows.
		name := f.Name()
		f.Close()
		os.Remove(name)
	}()

	resp, err = http.Get("https://go.dev/dl/" + file.Filename)
	if err != nil {
		return fmt.Errorf("getting release file: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching file, status %v, expected 200 OK", resp.Status)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("copying release file: %v", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		return fmt.Errorf("rewinding downloaded release file: %v", err)
	}
	if _, err := openpgp.CheckArmoredDetachedSignature(signingKey, f, bytes.NewReader(sigbuf)); err != nil {
		return fmt.Errorf("verifying pgp signature on go release: %v", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		return fmt.Errorf("rewinding downloaded release file after signature verification: %v", err)
	}

	if strings.HasSuffix(file.Filename, ".tar.gz") {
		return fetchTgz(f, file, dst, permissions)
	} else if strings.HasSuffix(file.Filename, ".zip") {
		return fetchZip(f, file, dst, permissions)
	}
	return fmt.Errorf("file extension not supported, only .tar.gz and .zip supported")
}

func dstName(dst, name string) (string, error) {
	if name != "go" && !strings.HasPrefix(name, "go/") {
		return "", fmt.Errorf("path %q: does not start with \"go\"", name)
	}

	r := filepath.Clean(filepath.Join(dst, name))
	if !strings.HasPrefix(r, dst) {
		return "", fmt.Errorf("bad path %q in archive, resulting in path %q outside dst %q", name, r, dst)
	}
	return r, nil
}
