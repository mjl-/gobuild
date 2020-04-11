package main

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/mjl-/goreleases"
)

var getLog func(string, ...interface{}) = func(format string, args ...interface{}) {}

func get(args []string) {
	flags := flag.NewFlagSet("get", flag.ExitOnError)

	var (
		verifierKey = flags.String("verifierkey", "beta.gobuilds.org+3979319f+AReBl47t6/Zl24/pmarcKhJtsfAU2c1F5Wtu4hrOgOQQ", "Verifier key for transparency log.")
		baseURL     = flags.String("url", "", "URL for lookups of hashes at the transparency log. If empty, this is set based on the name of the verifier key, using HTTPS if name contains a dot and plain HTTP otherwise.")
		verbose     = flags.Bool("verbose", false, "Print actions.")
		sum         = flags.String("sum", "", "Sum to verify.")
		bindir      = flags.String("bindir", ".", "Directory to store binary in.")
		target      = flags.String("target", "", "Target to retrieve binary for. Default is current GOOS/GOARCH.")
		goversion   = flags.String("goversion", "latest", `Go toolchain/SDK version. Default "latest" resolves through golang.org/dl/, caching results for 1 hour.`)
		download    = flags.Bool("download", true, "Download binary.")
		goproxy     = flags.String("goproxy", "https://proxy.golang.org", `Go proxy to use for resolving "latest" module versions.`)
	)

	flags.Usage = func() {
		log.Println("usage: gobuild get [flags] module@version/package")
		flags.PrintDefaults()
		os.Exit(2)
	}
	flags.Parse(args)
	args = flags.Args()
	if len(args) != 1 {
		flags.Usage()
	}

	if !strings.HasSuffix(*goproxy, "/") {
		*goproxy += "/"
	}

	if *verbose {
		getLog = func(format string, args ...interface{}) {
			log.Printf(format, args...)
		}
	}

	// Parse specifier from command-line.
	bs, err := parseGetSpec(args[0])
	if err != nil {
		log.Fatalf("parsing module@version/package: %v", err)
	}

	// Set goos & goarch based on -target or runtime.
	if *target == "" {
		bs.Goos = runtime.GOOS
		bs.Goarch = runtime.GOARCH
	} else {
		t := strings.Split(*target, "/")
		if len(t) != 2 {
			log.Fatal("bad target")
		}
		bs.Goos = t[0]
		bs.Goarch = t[1]
	}

	// Resolve latest version of go if needed.
	if *goversion == "latest" {
		getLog("resolving latest goversion")
		bs.Goversion, err = resolveLatestGoversion()
		if err != nil {
			log.Fatalf("resolving latest go version: %v", err)
		}
		getLog("latest goversion is %s", bs.Goversion)
	} else {
		bs.Goversion = *goversion
	}

	// Resolve latest module version at goproxy.
	if bs.Version == "latest" {
		getLog("resolving latest module version through goproxy")
		if modVer, err := resolveModuleLatest(context.Background(), *goproxy, bs.Mod); err != nil {
			log.Fatalf("resolving latest module: %v", err)
		} else {
			bs.Version = modVer.Version
			log.Printf("latest module version is %s", bs.Version)
		}
	}

	client, clientOps, err := newClient(*verifierKey, *baseURL)
	if err != nil {
		log.Fatalf("new client: %v", err)
	}
	key := bs.String()
	getLog("looking up key %s", key)
	_, data, err := client.Lookup(key)
	if err != nil {
		log.Fatalf("lookup: %v", err)
	}

	br, err := parseRecord(data)
	if err != nil {
		log.Fatalf("parsing record from remote: %v", err)
	}
	getLog("filesize %.1fmb, sum %s", float64(br.Filesize)/(1024*1024), br.Sum)

	rkey := br.String()
	if rkey != key {
		log.Fatalf("remote sent record for other key, got %s expected %s", rkey, key)
	}

	if *sum != "" {
		if *sum != br.Sum {
			log.Fatalf("remote has different sum %s, expected %s", br.Sum, *sum)
		}
		getLog("sum matches")
	}

	if !*download {
		return
	}

	gobuildBaseURL := clientOps.baseURL
	if strings.HasSuffix(gobuildBaseURL, "/tlog/") {
		gobuildBaseURL = gobuildBaseURL[:len(gobuildBaseURL)-len("tlog/")]
	}

	// Retrieve file to bindir with temp name, calculate checksum as we go.
	if f, err := ioutil.TempFile(*bindir, br.filename()+".gobuildget"); err != nil {
		log.Fatalf("creating temp file for downloading: %v", err)
	} else if err := fetch(f, gobuildBaseURL, br, *bindir); err != nil {
		if xerr := os.Remove(f.Name()); xerr != nil {
			log.Printf("removing tempfile %s: %v", f.Name(), xerr)
		}
		log.Fatal(err)
	}
}

func fetch(f *os.File, gobuildBaseURL string, br *buildResult, bindir string) error {
	link := gobuildBaseURL + request{br.buildSpec, br.Sum, pageDownloadGz}.link()[1:]
	getLog("downloading and verifying binary at %s", link)
	resp, err := http.Get(link)
	if err != nil {
		return fmt.Errorf("making request to download binary: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("remote http response for downloading binary: %s", resp.Status)
	}

	gzr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("gunzip binary: %v", err)
	}

	h := sha256.New()
	dst := io.MultiWriter(h, f)
	if _, err := io.Copy(dst, gzr); err != nil {
		return fmt.Errorf("downloading binary: %v", err)
	}
	if err := gzr.Close(); err != nil {
		return fmt.Errorf("close gzip stream: %v", err)
	}

	sha := h.Sum(nil)
	dlSum := "0" + base64.RawURLEncoding.EncodeToString(sha[:20])
	if dlSum != br.Sum {
		return fmt.Errorf("downloaded binary has sum %s, expected %s", dlSum, br.Sum)
	}
	getLog("sum of downloaded file matches")

	// Attempt to make file executable.
	info, err := f.Stat()
	if err != nil {
		log.Fatalf("stat temp file: %v", err)
	}
	// Set the "x" bit for the positions that have the "r" bit.
	mode := info.Mode() | (0111 & (info.Mode() >> 2))
	if err := f.Chmod(mode); err != nil && runtime.GOOS != "windows" {
		log.Printf("warning: making binary executable: %v", err)
	}

	tmpName := f.Name()
	if err := f.Close(); err != nil {
		return fmt.Errorf("close destination file: %v", err)
	}

	// Rename binary to final name.
	p := filepath.Join(bindir, br.filename())
	if err := os.Rename(tmpName, p); err != nil {
		return fmt.Errorf("rename to final destination: %v", err)
	}

	getLog("wrote %s", p)

	return nil
}

// Read cached latest goversion (1 hour age max), or retrieve through golang.org/dl/.
func resolveLatestGoversion() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}

	p := filepath.Join(dir, "gobuild", "get", "goversion")
	if f, err := os.Open(p); err == nil {
		defer f.Close()
		if info, err := f.Stat(); err != nil {
			return "", err
		} else if time.Since(info.ModTime()) < 1*time.Hour {
			getLog("latest goversion from cache at %s", p)
			buf, err := ioutil.ReadAll(f)
			return string(buf), err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}

	getLog("retrieving latest goversion through golang.org/dl/")
	rels, err := goreleases.ListSupported()
	if err != nil {
		return "", err
	}
	goversion := rels[0].Version
	os.MkdirAll(filepath.Dir(p), 0777) // error will show later
	if err := ioutil.WriteFile(p, []byte(goversion), 0666); err != nil {
		return "", err
	}
	return goversion, nil
}
