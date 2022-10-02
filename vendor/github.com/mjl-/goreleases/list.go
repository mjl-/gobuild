package goreleases

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Release is a released Go toolchain version, with files for several Os/Arch combinations.
type Release struct {
	Version string `json:"version"`
	Stable  bool   `json:"stable"`
	Files   []File `json:"files"`
}

// File is a released file for a released go version.
type File struct {
	Filename string `json:"filename"` // .tar.gz for unix-oriended files (source and binary), .pkg for macOS, .zip and .msi for Windows.
	Os       string `json:"os"`
	Arch     string `json:"arch"`
	Version  string `json:"version"`
	Sha256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Kind     string `json:"kind"` // "source", "archive", "package"
}

const urlCurrent = "https://go.dev/dl/?mode=json"
const urlAll = "https://go.dev/dl/?mode=json&include=all"

// ListSupported returns supported Go releases.
func ListSupported() ([]Release, error) {
	return list(urlCurrent)
}

// ListAll returns all Go releases, including historic.
func ListAll() ([]Release, error) {
	return list(urlAll)
}

func list(url string) ([]Release, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetching releases: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetching releases returned http status %d: %s", resp.StatusCode, resp.Status)
	}

	var rels []Release
	err = json.NewDecoder(resp.Body).Decode(&rels)
	if err != nil {
		return nil, fmt.Errorf("parsing releases JSON: %s", err)
	}
	// todo: add some validation for validity of content?

	return rels, nil
}
