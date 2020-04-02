package goreleases

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Permissions to set on extract files and directories, overriding permissions in the archive.
// Uid and gid are only set when at least one of them is >= 0. Setting uid/gid
// will fail on Windows.
type Permissions struct {
	Uid  int
	Gid  int
	Mode os.FileMode // Mode to use for extract files and directories. Files are masked with 0777 or 0666 depending on whether 0100 is set.
}

// Fetch downloads, extracts and verifies a Go release represented by file into directory dst.
// After a successful fetch, dst contains a directory "go" with the specified release.
// Directory dst must exist. It must not already contain a "go" subdirectory.
//
// Only files with filenames ending .tar.gz and .zip can be fetched. Tar.gz
// files are extracted while fetched. Zip files are first read into memory,
// then extracted.
//
// If permissions is not nil, it is applied to extracted files and directories.
func Fetch(file File, dst string, permissions *Permissions) error {
	if strings.HasSuffix(file.Filename, ".tar.gz") {
		return fetchTgz(file, dst, permissions)
	} else if strings.HasSuffix(file.Filename, ".zip") {
		return fetchZip(file, dst, permissions)
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
