package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func serveResult(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "405 - Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	req, hint, ok := parseRequest(r.URL.Path)
	if !ok {
		if hint != "" {
			http.Error(w, fmt.Sprintf("404 - File Not Found\n\n%s\n", hint), http.StatusNotFound)
		} else {
			http.NotFound(w, r)
		}
		return
	}
	defer observePage("result "+req.Page.String(), time.Now())

	lpath := filepath.Join(config.DataDir, "result", req.storeDir())

	bf, err := os.Open(filepath.Join(lpath, "build.json"))
	if err == nil {
		defer bf.Close()
	}
	var buildResult buildJSON
	if err == nil {
		err = json.NewDecoder(bf).Decode(&buildResult)
	}
	if err == nil && bytes.Equal(buildResult.SHA256, []byte{'x'}) {
		http.NotFound(w, r)
		return
	}
	if err != nil && !os.IsNotExist(err) {
		failf(w, "%w: reading build.json: %v", errServer, err)
		return
	}
	if err != nil {
		// Before attempting to build, check we don't have a failed build already.
		_, err = os.Stat(filepath.Join(lpath, "log.gz"))
		if err == nil {
			http.NotFound(w, r)
			return
		}

		// Attempt to build.
		err = prepareBuild(req)
		if err != nil {
			failf(w, "preparing build: %w", err)
			return
		}

		eventc := make(chan buildUpdate, 100)
		registerBuild(req, eventc)
		ctx := r.Context()

	loop:
		for {
			select {
			case <-ctx.Done():
				unregisterBuild(req, eventc)
				return
			case update := <-eventc:
				if !update.done {
					continue
				}
				unregisterBuild(req, eventc)
				if update.err != nil {
					failf(w, "build failed: %w", update.err)
					return
				}
				buildResult = *update.result
				break loop
			}
		}
	}

	if "0"+base64.RawURLEncoding.EncodeToString(buildResult.SHA256[:20]) != req.Sum {
		http.NotFound(w, r)
		return
	}

	switch req.Page {
	case pageLog:
		serveLog(w, r, filepath.Join(lpath, "log.gz"))
	case pageSha256:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "%x\n", buildResult.SHA256) // nothing to do for errors
	case pageDownloadRedirect:
		dreq := req
		dreq.Page = pageDownload
		http.Redirect(w, r, dreq.urlPath(), http.StatusTemporaryRedirect)
	case pageDownload:
		p := filepath.Join(lpath, req.downloadFilename()+".gz")
		f, err := os.Open(p)
		if err != nil {
			failf(w, "%w: open binary: %v", errServer, err)
			return
		}
		defer f.Close()
		serveGzipFile(w, r, p, f)
	case pageDownloadGz:
		p := filepath.Join(lpath, req.downloadFilename()+".gz")
		http.ServeFile(w, r, p)
	case pageBuildJSON:
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(buildResult) // nothing to do for errors
	case pageIndex:
		serveIndex(w, r, req, &buildResult)
	default:
		failf(w, "%w: unknown page %v", errServer, req.Page)
	}
}

func readGzipFile(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fgz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	return ioutil.ReadAll(fgz)
}
