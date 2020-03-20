package main

import (
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"time"
)

func build(w http.ResponseWriter, r *http.Request, req request) bool {
	start := time.Now()

	err := ensureSDK(req.Goversion)
	if err != nil {
		failf(w, "missing toolchain %q: %v", req.Goversion, err)
		return false
	}

	gobin := path.Join(config.SDKDir, req.Goversion, "bin/go")
	if !path.IsAbs(gobin) {
		gobin = path.Join(workdir, gobin)
	}
	_, err = os.Stat(gobin)
	if err != nil {
		failf(w, "unknown toolchain %q: %v", req.Goversion, err)
		return false
	}

	dir, err := ioutil.TempDir("", "gobuild")
	if err != nil {
		failf(w, "tempdir for build: %v", err)
		return false
	}
	defer os.RemoveAll(dir)

	homedir := config.HomeDir
	if !path.IsAbs(homedir) {
		homedir = path.Join(workdir, config.HomeDir)
	}
	os.Mkdir(homedir, 0777)

	cmd := exec.CommandContext(r.Context(), gobin, "get", req.Mod[:len(req.Mod)-1]+"@"+req.Version)
	cmd.Dir = dir
	cmd.Env = []string{
		fmt.Sprintf("GOPROXY=%s", config.GoProxy),
		"GO111MODULE=on",
		"HOME=" + homedir,
	}
	getOutput, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("output: %s", string(getOutput))
		failf(w, "fetching module: %v", err)
		return false
	}

	lname := dir + "/bin/" + req.filename()
	os.Mkdir(filepath.Dir(lname), 0777)
	cmd = exec.CommandContext(r.Context(), gobin, "build", "-o", lname, "-x", "-trimpath", "-ldflags", "-buildid=00000000000000000000/00000000000000000000/00000000000000000000/00000000000000000000")
	cmd.Env = []string{
		"CGO_ENABLED=0",
		"GOOS=" + req.Goos,
		"GOARCH=" + req.Goarch,
		"HOME=" + homedir,
	}
	cmd.Dir = homedir + "/go/pkg/mod/" + req.Mod[:len(req.Mod)-1] + "@" + req.Version
	if req.Dir != "" {
		cmd.Dir += "/" + req.Dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		failf(w, "running build: %v", err)
		log.Printf("output: %s", string(output))
		return false
	}

	err = saveFiles(req, output, lname, start, cmd.ProcessState.SystemTime(), cmd.ProcessState.UserTime())
	if err != nil {
		failf(w, "writing resulting files: %v", err)
		return false
	}
	return true
}

func saveFiles(req request, logOutput []byte, lname string, start time.Time, systemTime, userTime time.Duration) error {
	of, err := os.Open(lname)
	if err != nil {
		return err
	}
	defer of.Close()

	fi, err := of.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()

	h := sha256.New()
	_, err = io.Copy(h, of)
	if err != nil {
		return err
	}
	sha256 := fmt.Sprintf("%x", h.Sum(nil))
	sum := sha256[:40]
	_, err = of.Seek(0, io.SeekStart)
	if err != nil {
		return err
	}

	dir := path.Join(config.DataDir, req.destdir())

	success := false
	defer func() {
		if !success {
			os.RemoveAll(dir)
		}
	}()
	os.MkdirAll(dir, 0777)

	err = ioutil.WriteFile(dir+"/sha256", []byte(sha256), 0666)
	if err != nil {
		return err
	}

	lf, err := os.Create(dir + "/log.gz")
	if err != nil {
		return err
	}
	defer func() {
		if lf != nil {
			lf.Close()
		}
	}()
	lfgz := gzip.NewWriter(lf)
	_, err = lfgz.Write(logOutput)
	if err != nil {
		return err
	}
	err = lfgz.Close()
	if err != nil {
		return err
	}
	err = lf.Close()
	lf = nil
	if err != nil {
		return err
	}

	nf, err := os.Create(dir + "/" + sum + ".gz")
	if err != nil {
		return err
	}
	defer func() {
		if nf != nil {
			nf.Close()
		}
	}()
	nfgz := gzip.NewWriter(nf)
	_, err = io.Copy(nfgz, of)
	if err != nil {
		return err
	}
	err = nfgz.Close()
	if err != nil {
		return err
	}
	err = nf.Close()
	nf = nil
	if err != nil {
		return err
	}

	bf, err := os.OpenFile(path.Join(config.DataDir, "builds.txt"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() {
		if bf != nil {
			bf.Close()
		}
	}()
	_, err = fmt.Fprintf(bf, "v1 %s %d %d %d %d %d %s %s %s %s %s %s\n", sha256, size, start.UnixNano()/int64(time.Millisecond), time.Since(start)/time.Millisecond, systemTime/time.Millisecond, userTime/time.Millisecond, req.Goos, req.Goarch, req.Goversion, req.Mod, req.Version, req.Dir)
	if err != nil {
		return err
	}
	err = bf.Close()
	if err != nil {
		return err
	}

	success = true
	return nil
}
