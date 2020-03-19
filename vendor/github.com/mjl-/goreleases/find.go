package goreleases

import (
	"fmt"
)

// FindFile finds the file in a release for a given os, arch, kind.
// For empty values of os, arch, kind parameters, any file in the release matches.
func FindFile(release Release, os, arch, kind string) (File, error) {
	for _, f := range release.Files {
		if os != "" && f.Os != os {
			continue
		}
		if arch != "" && f.Arch != arch {
			continue
		}
		if kind != "" && f.Kind != kind {
			continue
		}
		return f, nil
	}
	return File{}, fmt.Errorf("file not found")
}
