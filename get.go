package main

import (
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
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/mjl-/goreleases"
)

var getLog func(string, ...interface{}) = func(format string, args ...interface{}) {}

func get(args []string) {
	flags := flag.NewFlagSet("get", flag.ExitOnError)

	verifierKey := flags.String("verifierkey", "beta.gobuilds.org+3979319f+AReBl47t6/Zl24/pmarcKhJtsfAU2c1F5Wtu4hrOgOQQ", "Verifier key for transparency log.")
	baseURL := flags.String("url", "", "URL for lookups of hashes at the transparency log. If empty, this is set based on the name of the verifier key, using HTTPS if name contains a dot and plain HTTP otherwise.")
	verbose := flags.Bool("verbose", false, "Print actions.")
	sum := flags.String("sum", "", "Sum to verify.")
	bindir := flags.String("bindir", ".", "Directory to store binary in.")
	target := flags.String("target", "", "Target to retrieve binary for. Default is current GOOS/GOARCH.")
	goversion := flags.String("goversion", "latest", `Go toolchain/SDK version. Default "latest" resolves through golang.org/dl/, caching results for 1 hour.`)
	download := flags.Bool("download", true, "Download binary.")
	goproxy := flags.String("goproxy", "https://proxy.golang.org", `Go proxy to use for resolving "latest" module versions.`)

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
	spec, err := parseSpec(args[0])
	if err != nil {
		log.Fatalf("parsing module@version/package: %v", err)
	}

	// Set goos & goarch based on -target or runtime.
	var goos, goarch string
	if *target == "" {
		goos = runtime.GOOS
		goarch = runtime.GOARCH
	} else {
		t := strings.Split(*target, "/")
		if len(t) != 2 {
			log.Fatal("bad target")
		}
		goos, goarch = t[0], t[1]
	}

	// Resolve latest version of go if needed.
	if *goversion == "latest" {
		getLog("resolving latest goversion")
		*goversion, err = resolveLatestGoversion()
		if err != nil {
			log.Fatalf("resolving latest go version: %v", err)
		}
		getLog("latest goversion is %s", *goversion)
	}

	// Resolve latest module version at goproxy.
	if spec.Version == "latest" {
		getLog("resolving latest module version through goproxy")
		modVer, err := resolveModuleLatest(context.Background(), *goproxy, spec.Mod)
		if err != nil {
			log.Fatalf("resolving latest module: %v", err)
		}
		spec.Version = modVer.Version
		log.Printf("latest module version is %s", spec.Version)
	}

	key := fmt.Sprintf("%s-%s-%s/%s@%s/%s", goos, goarch, *goversion, spec.Mod, spec.Version, spec.Dir)

	client, clientOps, err := newClient(*verifierKey, *baseURL)
	if err != nil {
		log.Fatalf("new client: %v", err)
	}
	getLog("looking up remote key %s", key)
	_, data, err := client.Lookup(key)
	if err != nil {
		log.Fatalf("lookup: %v", err)
	}

	msg := string(data)
	if !strings.HasSuffix(msg, "\n") {
		log.Fatalf("missing newline in record from remote")
	}
	msg = msg[:len(msg)-1]
	t := strings.Split(msg, " ")
	// Expecting: module version dir goos goarch goversion size sum
	if len(t) != 8 {
		log.Fatalf("bad record, got %d records, expected 8", len(t))
	}
	remoteSum := t[7]
	size, err := strconv.ParseInt(t[6], 10, 64)
	if err != nil {
		log.Fatalf("bad filesize %s: %v", t[6], err)
	}
	getLog("filesize %.1fmb, sum %s", float64(size)/(1024*1024), remoteSum)

	rkey := fmt.Sprintf("%s-%s-%s/%s@%s%s", t[3], t[4], t[5], t[0], t[1], t[2])
	if rkey != key {
		log.Fatalf("remote sent record for other key, got %s expected %s", rkey, key)
	}

	if *sum != "" {
		if *sum != remoteSum {
			log.Fatalf("remote has different sum %s, expected %s", remoteSum, *sum)
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
	var filename string
	if spec.Dir != "" {
		filename = path.Base(spec.Dir)
	} else {
		filename = path.Base(spec.Mod)
	}
	f, err := ioutil.TempFile(*bindir, filename+".gobuildget")
	if err != nil {
		log.Fatalf("creating temp file for downloading: %v", err)
	}
	err = fetch(f, gobuildBaseURL, spec, goos, goarch, *goversion, remoteSum, *bindir, filename)
	if err != nil {
		if err := os.Remove(f.Name()); err != nil {
			log.Printf("removing tempfile %s: %v", f.Name(), err)
		}
		log.Fatal(err)
	}
}

func fetch(f *os.File, gobuildBaseURL string, spec *spec, goos, goarch, goversion, remoteSum, bindir, filename string) error {
	link := fmt.Sprintf("%sr/%s@%s/%s%s-%s-%s/%s/dl", gobuildBaseURL, spec.Mod, spec.Version, spec.Dir, goos, goarch, goversion, remoteSum)
	getLog("downloading and verifying binary at %s", link)
	resp, err := http.Get(link)
	if err != nil {
		return fmt.Errorf("making request to download binary: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("remote http response for downloading binary: %s", resp.Status)
	}
	h := sha256.New()
	dst := io.MultiWriter(h, f)
	if _, err := io.Copy(dst, resp.Body); err != nil {
		return fmt.Errorf("downloading binary: %v", err)
	}
	sha := h.Sum(nil)
	dlSum := "0" + base64.RawURLEncoding.EncodeToString(sha[:20])
	if dlSum != remoteSum {
		return fmt.Errorf("downloaded binary has sum %s, expected %s", dlSum, remoteSum)
	}
	getLog("sum of downloaded file matches")

	// Attempt to make file executable.
	info, err := f.Stat()
	if err != nil {
		log.Fatalf("stat temp file: %v", err)
	}
	// Set the "x" bit for the positions that have the "r" bit.
	mode := info.Mode() | (0111 & (info.Mode() >> 2))
	if err := f.Chmod(mode); err != nil {
		log.Printf("warning: making binary executable: %v", err)
	}

	tmpName := f.Name()
	if err := f.Close(); err != nil {
		return fmt.Errorf("close destination file: %v", err)
	}

	// Rename binary to final name.
	p := filepath.Join(bindir, filename)
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
	f, err := os.Open(p)
	if err == nil {
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return "", err
		}
		if time.Since(info.ModTime()) < 1*time.Hour {
			getLog("latest goversion from cache at %s", p)
			buf, err := ioutil.ReadAll(f)
			return string(buf), err
		}
	}
	if err != nil && !os.IsNotExist(err) {
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

type spec struct {
	Mod, Version, Dir string
}

func parseSpec(s string) (*spec, error) {
	r := &spec{}
	t := strings.SplitN(s, "@", 2)
	if len(t) != 2 {
		r.Mod = s
		if path.Clean(r.Mod) != r.Mod {
			return nil, fmt.Errorf("non-canonical module directory")
		}
		r.Version = "latest"
		return r, nil
	}
	r.Mod = t[0]
	s = t[1]

	t = strings.SplitN(s, "/", 2)
	r.Version = t[0]
	if len(t) == 2 {
		r.Dir = t[1]
	}
	if r.Dir != "" && !strings.HasSuffix(r.Dir, "/") {
		return nil, fmt.Errorf("missing slash at end of package dir")
	}
	if r.Dir != "" && path.Clean(r.Dir)+"/" != r.Dir {
		return nil, fmt.Errorf("non-canonical package directory")
	}
	if path.Clean(r.Mod) != r.Mod {
		return nil, fmt.Errorf("non-canonical module directory")
	}
	return r, nil
}
