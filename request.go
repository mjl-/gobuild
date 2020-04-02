package main

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
)

type page int

const (
	pageIndex page = iota
	pageLog
	pageSha256
	pageDownloadRedirect
	pageDownload
	pageDownloadGz
	pageBuildJSON
	pageEvents
)

var pageNames = []string{"index", "log", "sha256", "dlredir", "download", "downloadgz", "buildjson", "events"}

func (p page) String() string {
	return pageNames[p]
}

type request struct {
	Mod       string // Eg github.com/mjl-/gobuild
	Version   string // Either explicit version like "v0.1.2", or "latest".
	Dir       string // Either empty, or ending with a slash.
	Goos      string
	Goarch    string
	Goversion string // Eg "go1.14.1" or "latest".
	Page      page
	Sum       string // If set, indicates a request on /r/.
}

func (r request) isBuild() bool {
	return r.Sum == ""
}

// GOBIN-relative name of file created by "go get". Used as key to prevent
// concurrent builds that would create the same output file. This does not take
// into account that compiles for the same GOOS/GOARCH as host will just write to
// $GOBIN.
func (r request) outputPath() string {
	var name string
	if r.Dir != "" {
		name = filepath.Base(r.Dir)
	} else {
		name = filepath.Base(r.Mod)
	}
	if r.Goos == "windows" {
		name += ".exe"
	}
	return fmt.Sprintf("%s-%s/%s", r.Goos, r.Goarch, name)
}

// buildRequest returns a request that points to a /b/ URL, leaving page intact.
func (r request) buildRequest() request {
	r.Sum = ""
	return r
}

func (r request) buildIndexRequest() request {
	r.Page = pageIndex
	r.Sum = ""
	return r
}

// local directory where results are stored.
func (r request) storeDir() string {
	return filepath.FromSlash(fmt.Sprintf("%s-%s-%s/%s@%s/%s", r.Goos, r.Goarch, r.Goversion, r.Mod, r.Version, r.Dir))
}

// Path in URL for this request.
func (r request) urlPath() string {
	var kind string
	if r.Sum == "" {
		kind = "b"
	} else {
		kind = "r"
	}
	s := fmt.Sprintf("/%s/%s@%s/%s%s-%s-%s/", kind, r.Mod, r.Version, r.Dir, r.Goos, r.Goarch, r.Goversion)
	if r.Sum != "" {
		s += r.Sum + "/"
	}
	s += r.pagePart()
	return s
}

func (r request) pagePart() string {
	switch r.Page {
	case pageIndex:
		return ""
	case pageLog:
		return "log"
	case pageSha256:
		return "sha256"
	case pageDownloadRedirect:
		return "dl"
	case pageDownload:
		return r.downloadFilename()
	case pageDownloadGz:
		return r.downloadFilename() + ".gz"
	case pageBuildJSON:
		return "build.json"
	case pageEvents:
		return "events"
	default:
		panic("missing case")
	}
}

func (r request) filename() string {
	if r.Dir != "" {
		return filepath.Base(r.Dir)
	}
	return filepath.Base(r.Mod)
}

// Name of file the http user-agent (browser) will save the file as.
func (r request) downloadFilename() string {
	ext := ""
	if r.Goos == "windows" {
		ext = ".exe"
	}
	return fmt.Sprintf("%s-%s-%s%s", r.filename(), r.Version, r.Goversion, ext)
}

// We'll get paths like /[br]/github.com/mjl-/sherpa@v0.6.0/cmd/sherpaclient/linux-amd64-go1.14.1/0m32pSahHbf-fptQdDyWD87GJNXI/{log,sha256,dl,<name>,<name>.gz,build.json, events}
func parseRequest(s string) (r request, hint string, ok bool) {
	withSum := strings.HasPrefix(s, "/r/")
	s = s[len("/X/"):]

	var downloadName string
	switch {
	case strings.HasSuffix(s, "/"):
		r.Page = pageIndex
		s = s[:len(s)-len("/")]
	case strings.HasSuffix(s, "/log"):
		r.Page = pageLog
		s = s[:len(s)-len("/log")]
	case strings.HasSuffix(s, "/sha256"):
		r.Page = pageSha256
		s = s[:len(s)-len("/sha256")]
	case strings.HasSuffix(s, "/dl"):
		r.Page = pageDownloadRedirect
		s = s[:len(s)-len("/dl")]
	case strings.HasSuffix(s, "/build.json"):
		r.Page = pageBuildJSON
		s = s[:len(s)-len("/build.json")]
	case strings.HasSuffix(s, "/events"):
		r.Page = pageEvents
		s = s[:len(s)-len("/events")]
	default:
		t := strings.Split(s, "/")
		downloadName = t[len(t)-1]
		if strings.HasSuffix(downloadName, ".gz") {
			r.Page = pageDownloadGz
		} else {
			r.Page = pageDownload
		}
		s = s[:len(s)-1-len(downloadName)]
		// After parsing module,version,package below, we'll check if the download name is
		// indeed valid for this filename.
	}

	// Remaining: example.com/user/repo@version/cmd/dir/goos-goarch-goversion/sum
	t := strings.SplitN(s, "@", 2)
	if len(t) != 2 {
		hint = "Perhaps missing @version?"
		return
	}
	r.Mod = t[0]
	s = t[1]

	// Remaining: version/cmd/dir/linux-amd64-go1.13/sum
	t = strings.Split(s, "/")
	var sum string
	if withSum {
		if len(t) < 3 {
			hint = "Perhaps missing sum?"
			return
		}
		sum = t[len(t)-1]
		if !strings.HasPrefix(sum, "0") {
			hint = "Perhaps malformed or misversioned sum?"
			return
		}
		xsum, err := base64.RawURLEncoding.DecodeString(sum[1:])
		if err != nil || len(xsum) != 20 {
			hint = "Perhaps malformed sum?"
			return
		}
		r.Sum = sum
		t = t[:len(t)-1]
	}
	if len(t) < 2 {
		hint = "Perhaps missing version or goos-goarch-goversion?"
		return
	}
	tt := strings.Split(t[len(t)-1], "-")
	if len(tt) != 3 {
		hint = "Perhaps bad goos-goarch-goversion?"
		return
	}
	r.Goos = tt[0]
	r.Goarch = tt[1]
	r.Goversion = tt[2]
	t = t[:len(t)-1]
	r.Version = t[0]
	r.Dir = strings.Join(t[1:], "/")
	if r.Dir != "" {
		r.Dir += "/"
	}

	switch r.Page {
	case pageDownload:
		if r.downloadFilename() != downloadName {
			return
		}
	case pageDownloadGz:
		if r.downloadFilename()+".gz" != downloadName {
			return
		}
	}

	if withSum && r.Page == pageEvents {
		return
	}

	ok = true
	return
}
