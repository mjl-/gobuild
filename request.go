package main

import (
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
	Sum  string // Empty for build instead of result requests: /b vs /r.
	Page page
}

// Path in URL for this request, for linking to other pages.
func (r request) link() string {
	var kind string
	if r.Sum == "" {
		kind = "b"
	} else {
		kind = "r"
	}
	s := fmt.Sprintf("/%s/%s@%s/%s%s-%s-%s/", kind, r.Mod, r.Version, r.appendDir(), r.Goos, r.Goarch, r.Goversion)
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

// We'll get paths like /[br]/github.com/mjl-/sherpa@v0.6.0/cmd/sherpaclient/linux-amd64-go1.14.1/0m32pSahHbf-fptQdDyWD87GJNXI/{log,dl,<name>,<name>.gz,record, events}
func parseRequest(s string) (r request, hint string, ok bool) {
	withSum := strings.HasPrefix(s, "/r/")
	s = s[len("/X/"):]

	if s == "" {
		hint = ""
		return
	}

	// First peel off the trailing (optional) page, and possible sum.
	var page string
	if !strings.HasSuffix(s, "/") {
		t := strings.Split(s, "/")
		page = t[len(t)-1]
		s = s[:len(s)-len(page)]
		if !strings.HasSuffix(s, "/") {
			hint = "No path left after peeling off page from end"
			return
		}
	}
	if withSum {
		t := strings.Split(s[:len(s)-1], "/")
		r.Sum = t[len(t)-1]
		if len(r.Sum) != 28 {
			hint = "malformed sum"
			return
		}
		s = s[:len(s)-len(r.Sum)-1]
		if !strings.HasSuffix(s, "/") {
			hint = "No path left after peeling off sum from end"
			return
		}
	}

	// No we have a regular buildspec left.
	if bs, err := parseBuildSpec(s); err != nil {
		hint = "Bad module@version/path: " + err.Error()
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
			return
		}
	}

	if withSum && r.Page == pageEvents {
		hint = "No events endpoint for results"
		return
	}

	ok = true
	return
}
