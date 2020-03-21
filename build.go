package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

type buildJSON struct {
	V             string // "v0"
	SHA256        []byte
	Filesize      int64
	FilesizeGz    int64
	Start         time.Time
	BuildWallTime time.Duration
	SystemTime    time.Duration
	UserTime      time.Duration
	Goversion     string
	Goos          string
	Goarch        string
	Mod           string
	Version       string
	Dir           string
}

func build(w http.ResponseWriter, r *http.Request, req request) (result *buildJSON, tmpFail bool) {
	start := time.Now()

	err := ensureSDK(req.Goversion)
	if err != nil {
		failf(w, "missing toolchain %q: %v", req.Goversion, err)
		return nil, true
	}

	gobin := path.Join(config.SDKDir, req.Goversion, "bin/go")
	if !path.IsAbs(gobin) {
		gobin = path.Join(workdir, gobin)
	}
	_, err = os.Stat(gobin)
	if err != nil {
		failf(w, "unknown toolchain %q: %v", req.Goversion, err)
		return nil, true
	}

	dir, err := ioutil.TempDir("", "gobuild")
	if err != nil {
		failf(w, "tempdir for build: %v", err)
		return nil, true
	}
	defer os.RemoveAll(dir)

	modDir, getOutput, err := ensureModule(gobin, req.Mod, req.Version)
	if err != nil {
		// Fetching the code failed. We report it back to the user immediately. We don't
		// store these results. Perhaps the user is trying to build something that was
		// uploaded just now, and not yet available in the go module proxy.
		ufailf(w, "error fetching module from goproxy: %v\n\n# output from go get:\n%s", err, string(getOutput))
		return nil, true
	}

	pkgDir := modDir + "/" + req.Dir

	// Check if package is a main package, resulting in an executable when built.
	cmd := exec.CommandContext(r.Context(), gobin, "list", "-f", "{{.Name}}")
	cmd.Dir = pkgDir
	cmd.Env = []string{
		fmt.Sprintf("GOPROXY=%s", config.GoProxy),
		"GO111MODULE=on",
		"HOME=" + homedir,
	}
	nameOutput, err := cmd.CombinedOutput()
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "400 - error finding package name; perhaps package does not exist?")
		w.Write(nameOutput) // nothing to do for errors
		return nil, true
	}
	if string(nameOutput) != "main\n" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "400 - not package main, building would not result in executable binary")
		return nil, true
	}

	lname := dir + "/bin/" + req.filename()
	os.Mkdir(filepath.Dir(lname), 0775) // failures will be caught later
	cmd = exec.CommandContext(r.Context(), gobin, "build", "-mod=readonly", "-o", lname, "-x", "-v", "-trimpath", "-ldflags", "-buildid=00000000000000000000/00000000000000000000/00000000000000000000/00000000000000000000")
	cmd.Env = []string{
		fmt.Sprintf("GOPROXY=%s", config.GoProxy),
		"GO111MODULE=on",
		"CGO_ENABLED=0",
		"GOOS=" + req.Goos,
		"GOARCH=" + req.Goarch,
		"HOME=" + homedir,
	}
	cmd.Dir = pkgDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		var sysTime, userTime time.Duration
		if cmd.ProcessState != nil {
			sysTime = cmd.ProcessState.SystemTime()
			userTime = cmd.ProcessState.UserTime()
		}
		err := saveFailure(req, err.Error()+"\n\n"+string(output), start, sysTime, userTime)
		if err != nil {
			failf(w, "storing results of failure: %v", err)
			return nil, true
		}
		return nil, false
	}

	result, err = saveFiles(req, output, lname, start, cmd.ProcessState.SystemTime(), cmd.ProcessState.UserTime())
	if err != nil {
		failf(w, "storing results: %v", err)
		return nil, true
	}
	return result, false
}

func saveFailure(req request, output string, start time.Time, systemTime, userTime time.Duration) error {
	tmpdir, err := ioutil.TempDir(config.DataDir, "failure")
	if err != nil {
		return err
	}
	defer func() {
		if tmpdir != "" {
			os.RemoveAll(tmpdir)
		}
	}()

	err = writeGz(tmpdir+"/log.gz", strings.NewReader(output))
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

	buildResult := buildJSON{
		"v0",
		nil, // Marks failure.
		0,
		0,
		start,
		time.Since(start),
		systemTime,
		userTime,
		req.Goversion,
		req.Goos,
		req.Goarch,
		req.Mod,
		req.Version,
		req.Dir,
	}
	err = appendBuildsTxt(buildResult)
	return err
}

func saveFiles(req request, output []byte, lname string, start time.Time, systemTime, userTime time.Duration) (*buildJSON, error) {
	of, err := os.Open(lname)
	if err != nil {
		return nil, err
	}
	defer of.Close()

	fi, err := of.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()

	h := sha256.New()
	_, err = io.Copy(h, of)
	if err != nil {
		return nil, err
	}
	sha256 := h.Sum(nil)
	_, err = of.Seek(0, io.SeekStart)
	if err != nil {
		return nil, err
	}

	tmpdir, err := ioutil.TempDir(config.DataDir, "success")
	if err != nil {
		return nil, err
	}
	defer func() {
		if tmpdir != "" {
			os.RemoveAll(tmpdir)
		}
	}()

	buildResult := buildJSON{
		"v0",
		sha256,
		size,
		0, // filled in below
		start,
		time.Since(start),
		systemTime,
		userTime,
		req.Goversion,
		req.Goos,
		req.Goarch,
		req.Mod,
		req.Version,
		req.Dir,
	}

	err = writeGz(tmpdir+"/log.gz", bytes.NewReader(output))
	if err != nil {
		return nil, err
	}

	binGz := tmpdir + "/" + req.downloadFilename() + ".gz"
	err = writeGz(binGz, of)
	if err != nil {
		return nil, err
	}
	fi, err = os.Stat(binGz)
	if err != nil {
		return nil, err
	}
	buildResult.FilesizeGz = fi.Size()

	buf, err := json.Marshal(buildResult)
	if err != nil {
		return nil, fmt.Errorf("marshal build.json: %v", err)
	}
	err = ioutil.WriteFile(tmpdir+"/build.json", buf, 0664)
	if err != nil {
		return nil, err
	}

	finalDir := path.Join(config.DataDir, req.destdir())
	os.MkdirAll(path.Dir(finalDir), 0775) // failures will be caught later
	err = os.Rename(tmpdir, finalDir)
	if err != nil {
		return nil, err
	}
	tmpdir = ""

	err = appendBuildsTxt(buildResult)
	if err != nil {
		return nil, err
	}
	return &buildResult, nil
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

func appendBuildsTxt(b buildJSON) error {
	bf, err := os.OpenFile(path.Join(config.DataDir, "builds.txt"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() {
		if bf != nil {
			bf.Close()
		}
	}()
	sum := "x"
	if b.SHA256 != nil {
		sum = fmt.Sprintf("%x", b.SHA256)
	}
	_, err = fmt.Fprintf(bf, "v0 %s %d %d %d %d %d %d %s %s %s %s %s %s\n", sum, b.Filesize, b.FilesizeGz, b.Start.UnixNano()/int64(time.Millisecond), b.BuildWallTime/time.Millisecond, b.SystemTime/time.Millisecond, b.UserTime/time.Millisecond, b.Goos, b.Goarch, b.Goversion, b.Mod, b.Version, b.Dir)
	if err != nil {
		return err
	}
	err = bf.Close()
	bf = nil
	return err
}
