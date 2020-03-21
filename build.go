package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"time"
)

func build(w http.ResponseWriter, r *http.Request, req request) (ok bool, tmpFail bool) {
	start := time.Now()

	err := ensureSDK(req.Goversion)
	if err != nil {
		failf(w, "missing toolchain %q: %v", req.Goversion, err)
		return false, true
	}

	gobin := path.Join(config.SDKDir, req.Goversion, "bin/go")
	if !path.IsAbs(gobin) {
		gobin = path.Join(workdir, gobin)
	}
	_, err = os.Stat(gobin)
	if err != nil {
		failf(w, "unknown toolchain %q: %v", req.Goversion, err)
		return false, true
	}

	dir, err := ioutil.TempDir("", "gobuild")
	if err != nil {
		failf(w, "tempdir for build: %v", err)
		return false, true
	}
	defer os.RemoveAll(dir)

	homedir := config.HomeDir
	if !path.IsAbs(homedir) {
		homedir = path.Join(workdir, config.HomeDir)
	}
	os.Mkdir(homedir, 0775) // failures will be caught later

	cmd := exec.CommandContext(r.Context(), gobin, "get", "-d", "-v", req.Mod[:len(req.Mod)-1]+"@"+req.Version)
	cmd.Dir = dir
	cmd.Env = []string{
		fmt.Sprintf("GOPROXY=%s", config.GoProxy),
		"GO111MODULE=on",
		"HOME=" + homedir,
	}
	getOutput, err := cmd.CombinedOutput()
	if err != nil {
		// Fetching the code failed. We report it back to the user immediately. We don't
		// store these results. Perhaps the user is trying to build something that was
		// uploaded just now, and not yet available in the go module proxy.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(400)
		fmt.Fprintf(w, "400 - error fetching module from goproxy: %v\n\n# output:\n", err)
		w.Write(getOutput) // nothing to do for errors
		return false, true
	}

	lname := dir + "/bin/" + req.filename()
	os.Mkdir(filepath.Dir(lname), 0775) // failures will be caught later
	cmd = exec.CommandContext(r.Context(), gobin, "build", "-mod=readonly", "-o", lname, "-x", "-trimpath", "-ldflags", "-buildid=00000000000000000000/00000000000000000000/00000000000000000000/00000000000000000000")
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
		err := saveFailure(req, output, start, cmd.ProcessState.SystemTime(), cmd.ProcessState.UserTime())
		if err != nil {
			failf(w, "storing results: %v", err)
			return false, true
		}
		return false, false
	}

	err = saveFiles(req, output, lname, start, cmd.ProcessState.SystemTime(), cmd.ProcessState.UserTime())
	if err != nil {
		failf(w, "storing results: %v", err)
		return false, true
	}
	return true, false
}

func saveFailure(req request, output []byte, start time.Time, systemTime, userTime time.Duration) error {
	tmpdir, err := ioutil.TempDir(config.DataDir, "failure")
	if err != nil {
		return err
	}
	defer func() {
		if tmpdir != "" {
			os.RemoveAll(tmpdir)
		}
	}()

	err = writeGz(tmpdir+"/log.gz", bytes.NewReader(output))
	if err != nil {
		return err
	}

	finalDir := path.Join(config.DataDir, req.destdir())
	os.MkdirAll(path.Dir(finalDir), 0775) // failures will be caught later
	err = os.Rename(tmpdir, finalDir)
	if err != nil {
		return err
	}
	tmpdir = ""

	sha256 := "x" // Marks failure.
	size := int64(0)
	err = appendBuildsTxt(sha256, size, start, systemTime, userTime, req)
	return err
}

func saveFiles(req request, output []byte, lname string, start time.Time, systemTime, userTime time.Duration) error {
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

	tmpdir, err := ioutil.TempDir(config.DataDir, "success")
	if err != nil {
		return err
	}
	defer func() {
		if tmpdir != "" {
			os.RemoveAll(tmpdir)
		}
	}()

	err = ioutil.WriteFile(tmpdir+"/sha256", []byte(sha256), 0666)
	if err != nil {
		return err
	}

	err = writeGz(tmpdir+"/log.gz", bytes.NewReader(output))
	if err != nil {
		return err
	}

	err = writeGz(tmpdir+"/"+sum+".gz", of)
	if err != nil {
		return err
	}

	finalDir := path.Join(config.DataDir, req.destdir())
	os.MkdirAll(path.Dir(finalDir), 0775) // failures will be caught later
	err = os.Rename(tmpdir, finalDir)
	if err != nil {
		return err
	}
	tmpdir = ""

	err = appendBuildsTxt(sha256, size, start, systemTime, userTime, req)
	return err
}

func writeGz(path string, src io.Reader) error {
	lf, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if lf != nil {
			lf.Close()
		}
	}()
	lfgz := gzip.NewWriter(lf)
	_, err = io.Copy(lfgz, src)
	if err != nil {
		return err
	}
	err = lfgz.Close()
	if err != nil {
		return err
	}
	err = lf.Close()
	lf = nil
	return err
}

func appendBuildsTxt(sha256 string, size int64, start time.Time, systemTime, userTime time.Duration, req request) error {
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
	bf = nil
	return err
}
