package main

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Once gobuild is out of beta, this will be the verifier key for gobuilds.org.
const gobuildsOrgVerifierKey = "notyet"

var getLog func(string, ...interface{}) = func(format string, args ...interface{}) {}

func get(args []string) {
	flags := flag.NewFlagSet("get", flag.ExitOnError)

	var (
		verifierKey = flags.String("verifierkey", gobuildsOrgVerifierKey, "Verifier key for transparency log.")
		baseURL     = flags.String("url", "", "URL for lookups of hashes at the transparency log. If empty, this is set based on the name of the verifier key, using HTTPS if name contains a dot and plain HTTP otherwise.")
		verbose     = flags.Bool("verbose", false, "Print actions.")
		sum         = flags.String("sum", "", "Sum to verify.")
		bindir      = flags.String("bindir", ".", "Directory to store binary in.")
		target      = flags.String("target", "", "Target to retrieve binary for. Default is current GOOS/GOARCH.")
		goversion   = flags.String("goversion", "latest", `Go toolchain/SDK version. Default "latest" resolves through go.dev/dl/, caching results for 1 hour.`)
		download    = flags.Bool("download", true, "Download binary.")
		goproxy     = flags.String("goproxy", "https://proxy.golang.org", `Go proxy to use for resolving "latest" module versions.`)
		stripped    = flags.Bool("stripped", false, "Retrieve binary without symbol table and debug information.")
		quiet       = flags.Bool("quiet", false, "Do not print path that is written.")
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
	bs.Goversion = *goversion

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
	if *stripped {
		bs.Stripped = true
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

	rkey := br.String()
	if rkey != key && *sum != "" {
		log.Fatalf("lookup resolved to %s", rkey)
	}

	if *sum != "" {
		if *sum != br.Sum {
			log.Fatalf("remote has different sum %s, expected %s", br.Sum, *sum)
		}
		getLog("sum matches")
	}

	if (rkey != key || *sum == "") && !*quiet {
		log.Printf("resolved to %s, sum %s", rkey, br.Sum)
	}

	if !*download {
		return
	}

	dst := filepath.Join(*bindir, br.filename())
	if !*quiet {
		log.Printf("writing to %s, size %.1fmb", dst, float64(br.Filesize)/(1024*1024))
	}
	if _, err := os.Stat(dst); err == nil {
		log.Fatalf("aborted: destination path %s already exists", dst)
	}

	gobuildBaseURL := strings.TrimSuffix(clientOps.baseURL, "/tlog")

	// Retrieve file to bindir with temp name, calculate checksum as we go.
	if f, err := os.CreateTemp(*bindir, br.filename()+".gobuildget"); err != nil {
		log.Fatalf("creating temp file for downloading: %v", err)
	} else if err := fetch(f, gobuildBaseURL, br, dst); err != nil {
		if xerr := os.Remove(f.Name()); xerr != nil {
			log.Printf("removing tempfile %s: %v", f.Name(), xerr)
		}
		log.Fatal(err)
	}
}

func fetch(f *os.File, gobuildBaseURL string, br *buildResult, dst string) error {
	link := gobuildBaseURL + request{br.buildSpec, br.Sum, pageDownloadGz}.link()
	getLog("downloading and verifying binary at %s", link)
	resp, err := httpGet(link)
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
	df := io.MultiWriter(h, f)
	if _, err := io.Copy(df, gzr); err != nil {
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
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("rename to final destination: %v", err)
	}

	getLog("wrote %s", dst)

	return nil
}
