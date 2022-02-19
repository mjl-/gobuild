package main

import (
	"encoding/base64"
	"fmt"
	"path"
	"strings"
)

type page int

const (
	pageIndex page = iota
	pageLog
	pageDownloadRedirect
	pageDownload
	pageDownloadGz
	pageRecord
	pageEvents
)

func (p page) String() string {
	switch p {
	case pageIndex:
		return "index"
	case pageLog:
		return "log"
	case pageDownloadRedirect:
		return "dlredir"
	case pageDownload:
		return "download"
	case pageDownloadGz:
		return "downloadgz"
	case pageRecord:
		return "record"
	case pageEvents:
		return "events"
	}
	panic("missing case")
}

type request struct {
	buildSpec
	Sum  string // Empty for build instead of result requests.
	Page page
}

// Path in URL for this request, for linking to other pages.
func (r request) link() string {
	s := fmt.Sprintf("/%s@%s/%s%s-%s-%s/", r.Mod, r.Version, r.appendDir(), r.Goos, r.Goarch, r.Goversion)
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
	case pageDownloadRedirect:
		return "dl"
	case pageDownload:
		return r.downloadFilename()
	case pageDownloadGz:
		return r.downloadFilename() + ".gz"
	case pageRecord:
		return "record"
	case pageEvents:
		return "events"
	default:
		panic("missing case")
	}
}

// Name of file the browser will save the file as.
func (r request) downloadFilename() string {
	var name string
	if r.Dir != "/" {
		name = path.Base(r.Dir)
	} else {
		name = path.Base(r.Mod)
	}
	ext := ""
	if r.Goos == "windows" {
		ext = ".exe"
	}
	return fmt.Sprintf("%s-%s-%s%s", name, r.Version, r.Goversion, ext)
}

func isSum(s string) bool {
	if !strings.HasPrefix(s, "0") {
		return false
	}
	buf, err := base64.RawURLEncoding.DecodeString(s[1:])
	if err != nil {
		return false
	}
	return len(buf) == 20
}

// We'll get paths like /github.com/mjl-/sherpa@v0.6.0/cmd/sherpaclient/linux-amd64-go1.14.1/0m32pSahHbf-fptQdDyWD87GJNXI/{log,dl,<name>,<name>.gz,record, events}
// with optional sum.
func parseRequest(s string) (r request, hint string, ok bool) {
	if s == "" {
		hint = ""
		return
	}

	t := strings.Split(s[1:], "/")

	// Look for a page-part and take it off.
	var page string
	if t[len(t)-1] == "" {
		t = t[:len(t)-1]
	} else {
		page = t[len(t)-1]
		t = t[:len(t)-1]
	}
	if len(t) > 2 && isSum(t[len(t)-1]) {
		r.Sum = t[len(t)-1]
		t = t[:len(t)-1]
	}
	if len(t) < 2 {
		hint = "Missing goos-goarch-goversion at end of path"
		return
	}

	// Now we have a regular buildspec left.
	if bs, err := parseBuildSpec(strings.Join(t, "/") + "/"); err != nil {
		hint = "Bad module@version/path: " + err.Error() + ", or missing slash at end of URL"
		return
	} else {
		r.buildSpec = bs
	}

	switch page {
	case "":
		r.Page = pageIndex
	case "log":
		r.Page = pageLog
	case "dl":
		r.Page = pageDownloadRedirect
	case "record":
		r.Page = pageRecord
	case "events":
		r.Page = pageEvents
	default:
		dl := r.downloadFilename()
		if page == dl {
			r.Page = pageDownload
		} else if page == dl+".gz" {
			r.Page = pageDownloadGz
		} else {
			hint = "Missing slash at end of URL or unknown build/result page"
			return
		}
	}

	if r.Sum != "" && r.Page == pageEvents {
		hint = "No events endpoint for results"
		return
	}

	ok = true
	return
}
