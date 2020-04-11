package main

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

type buildSpec struct {
	Mod       string // E.g. github.com/mjl-/gobuild. Never starts or ends with slash, and is never empty.
	Version   string
	Dir       string // Always starts with slash. Never ends with slash unless "/".
	Goos      string
	Goarch    string
	Goversion string
}

// filename to store the binary as. With .exe for windows.
func (bs buildSpec) filename() string {
	var name string
	if bs.Dir != "/" {
		name = path.Base(bs.Dir)
	} else {
		name = path.Base(bs.Mod)
	}
	if bs.Goos == "windows" {
		name += ".exe"
	}
	return name
}

// Variant of Dir that is either empty or otherwise has no leading but does have a
// trailing slash. Makes it easier to make some clean path by simple concatenation.
// Returns eg "" or "cmd/x/".
func (bs buildSpec) appendDir() string {
	if bs.Dir == "/" {
		return ""
	}
	return bs.Dir[1:] + "/"
}

// Used in transparency log lookups, and used to calculate directory where build results are stored.
// Can be parsed with parseBuildSpec.
func (bs buildSpec) String() string {
	return fmt.Sprintf("%s@%s/%s%s-%s-%s/", bs.Mod, bs.Version, bs.appendDir(), bs.Goos, bs.Goarch, bs.Goversion)
}

// GOBIN-relative name of file created by "go get". Used as key to prevent
// concurrent builds that would create the same output file. This does not take
// into account that compiles for the same GOOS/GOARCH as host will just write to
// $GOBIN.
func (bs buildSpec) outputPath() string {
	var name string
	if bs.Dir != "/" {
		name = filepath.Base(bs.Dir)
	} else {
		name = filepath.Base(bs.Mod)
	}
	if bs.Goos == "windows" {
		name += ".exe"
	}
	return fmt.Sprintf("%s-%s/%s", bs.Goos, bs.Goarch, name)
}

// Local directory where results are stored, both successful and failed.
func (bs buildSpec) storeDir() string {
	sha := sha256.Sum256([]byte(bs.String()))
	sum := base64.RawURLEncoding.EncodeToString(sha[:20])
	return filepath.Join(resultDir, sum[:1], sum)
}

type buildResult struct {
	buildSpec
	Filesize int64
	Sum      string
}

// Parse string of the form: module@version/dir/goos-goarch-goversion/.
// String generates strings that parseBuildSpec parses.
func parseBuildSpec(s string) (buildSpec, error) {
	bs := buildSpec{}

	// First peel off goos-goarch-goversion/ from end.
	if !strings.HasSuffix(s, "/") {
		return bs, fmt.Errorf("missing trailing slash")
	}
	s = s[:len(s)-1]
	t := strings.Split(s, "/")
	last := t[len(t)-1]
	s = s[:len(s)-len(last)]

	t = strings.Split(last, "-")
	if len(t) != 3 {
		return bs, fmt.Errorf("bad goos-goarch-goversion")
	}
	bs.Goos = t[0]
	bs.Goarch = t[1]
	bs.Goversion = t[2]

	t = strings.SplitN(s, "@", 2)
	if len(t) != 2 {
		return bs, fmt.Errorf("missing @ version")
	}
	bs.Mod = t[0]
	s = t[1]

	t = strings.SplitN(s, "/", 2)
	if len(t) != 2 {
		return bs, fmt.Errorf("missing slash for package dir")
	}
	bs.Version = t[0]
	bs.Dir = "/" + t[1]
	if bs.Dir != "/" {
		if !strings.HasSuffix(bs.Dir, "/") {
			return bs, fmt.Errorf("missing slash at end of package dir")
		} else {
			bs.Dir = bs.Dir[:len(bs.Dir)-1]
		}
	}
	if path.Clean(bs.Mod) != bs.Mod {
		return bs, fmt.Errorf("non-canonical module name %q", bs.Mod)
	}
	if path.Clean(bs.Dir) != bs.Dir {
		return bs, fmt.Errorf("non-canonical package dir %q", bs.Dir)
	}

	return bs, nil
}

// Parse module[@version/dir].
func parseGetSpec(s string) (buildSpec, error) {
	bs := buildSpec{}

	t := strings.SplitN(s, "@", 2)
	if len(t) != 2 {
		bs.Mod = s
		if path.Clean(bs.Mod) != bs.Mod {
			return bs, fmt.Errorf("non-canonical module directory")
		}
		bs.Version = "latest"
		bs.Dir = "/"
		return bs, nil
	}
	bs.Mod = t[0]
	s = t[1]

	t = strings.SplitN(s, "/", 2)
	bs.Version = t[0]
	bs.Dir = "/"
	if len(t) == 2 {
		bs.Dir += strings.TrimRight(t[1], "/")
	}
	if path.Clean(bs.Mod) != bs.Mod {
		return bs, fmt.Errorf("non-canonical module name %q", bs.Mod)
	}
	if path.Clean(bs.Dir) != bs.Dir {
		return bs, fmt.Errorf("non-canonical package dir %q", bs.Dir)
	}
	return bs, nil
}

func parseRecord(data []byte) (*buildResult, error) {
	msg := string(data)
	if !strings.HasSuffix(msg, "\n") {
		return nil, fmt.Errorf("does not end in newline")
	}
	msg = msg[:len(msg)-1]
	t := strings.Split(msg, " ")
	if len(t) != 8 {
		return nil, fmt.Errorf("bad record, got %d records, expected 8", len(t))
	}
	size, err := strconv.ParseInt(t[6], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("bad filesize %s: %v", t[6], err)
	}
	br := &buildResult{buildSpec{t[0], t[1], t[2], t[3], t[4], t[5]}, size, t[7]}
	return br, nil
}

func (br buildResult) packRecord() ([]byte, error) {
	fields := []string{
		br.Mod,
		br.Version,
		br.Dir,
		br.Goos,
		br.Goarch,
		br.Goversion,
		fmt.Sprintf("%d", br.Filesize),
		br.Sum,
	}
	for i, f := range fields {
		if f == "" {
			return nil, fmt.Errorf("bad empty field %d", i)
		}
		for _, c := range f {
			if c <= ' ' {
				return nil, fmt.Errorf("bad field %d in record: %q", i, f)
			}
		}
	}
	if len(br.Sum) != 28 {
		return nil, fmt.Errorf("bad length for sum")
	}
	if br.Filesize == 0 {
		return nil, fmt.Errorf("bad filesize 0")
	}
	return []byte(strings.Join(fields, " ") + "\n"), nil
}
