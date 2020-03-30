package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
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

var errTempFailure = errors.New("temporary failure")

type buildJSON struct {
	V             string // "v0"
	Sum           string // Sum is the versioned raw-base64-url encoded 20-byte prefix of the SHA256 sum. For v0, it starts with "0".
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

func ensureGobin(req request) (string, error) {
	gobin := path.Join(config.SDKDir, req.Goversion, "bin/go")
	if !path.IsAbs(gobin) {
		gobin = path.Join(workdir, gobin)
	}
	_, err := os.Stat(gobin)
	if err != nil {
		return "", fmt.Errorf("unknown toolchain %q: %v", req.Goversion, err)
	}
	return gobin, nil
}

func prepareBuild(req request) error {
	err := ensureSDK(req.Goversion)
	if err != nil {
		return fmt.Errorf("missing toolchain %q: %v", req.Goversion, err)
	}

	gobin, err := ensureGobin(req)
	if err != nil {
		return err
	}

	modDir, getOutput, err := ensureModule(gobin, req.Mod, req.Version)
	if err != nil {
		return fmt.Errorf("error fetching module from goproxy: %w\n\n# output from go get:\n%s", err, string(getOutput))
	}

	pkgDir := modDir + "/" + req.Dir

	// Check if package is a main package, resulting in an executable when built.
	cmd := exec.Command(gobin, "list", "-f", "{{.Name}}")
	cmd.Dir = pkgDir
	cmd.Env = []string{
		fmt.Sprintf("GOPROXY=%s", config.GoProxy),
		"GO111MODULE=on",
		"HOME=" + homedir,
	}
	nameOutput, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("error finding package name; perhaps package does not exist: %v\n\n# output from go list:\n%s", err, string(nameOutput))
	}
	if string(nameOutput) != "main\n" {
		return fmt.Errorf("not package main, building would not result in executable binary")
	}
	return nil
}

// goBuild performs the actual build. This is called from coordinate.go, not too
// many at a time.
func goBuild(req request) (*buildJSON, error) {
	bu := req.buildIndexRequest().urlPath()

	result, err := build(req)

	ok := err == nil && result != nil
	if err == nil || !errors.Is(err, errTempFailure) {
		availableBuilds.Lock()
		availableBuilds.index[bu] = ok
		availableBuilds.Unlock()
	}
	if err != nil {
		return nil, err
	}

	fp := bu
	if ok {
		rreq := req.buildIndexRequest()
		rreq.Sum = "0" + base64.RawURLEncoding.EncodeToString(result.SHA256[:20])
		fp = rreq.urlPath()
	}
	recentBuilds.Lock()
	recentBuilds.paths = append(recentBuilds.paths, fp)
	if len(recentBuilds.paths) > 10 {
		recentBuilds.paths = recentBuilds.paths[len(recentBuilds.paths)-10:]
	}
	recentBuilds.Unlock()
	return result, nil
}

func build(req request) (result *buildJSON, err error) {
	start := time.Now()

	gobin, err := ensureGobin(req)
	if err != nil {
		return nil, fmt.Errorf("ensuring go version is available: %v (%w)", err, errTempFailure)
	}

	modDir, getOutput, err := ensureModule(gobin, req.Mod, req.Version)
	if err != nil {
		return nil, fmt.Errorf("error fetching module from goproxy: %v (%w)\n\n# output from go get:\n%s", err, errTempFailure, string(getOutput))
	}
	pkgDir := modDir + "/" + req.Dir

	dir, err := ioutil.TempDir("", "gobuild")
	if err != nil {
		return nil, fmt.Errorf("tempdir for build: %v (%w)", err, errTempFailure)
	}
	defer os.RemoveAll(dir)

	// Launch goroutines to let them build the same code and return their build.json.
	// After our build, we'll verify if we all had the same result. If our build fails,
	// we'll just ignore these results, and let the remote builds continue. They will
	// not cancel the build anyway.

	type remoteBuild struct {
		verifyURL string
		err       error
		result    *buildJSON
	}

	verifyResult := make(chan remoteBuild, len(config.VerifierURLs))
	breq := req.buildRequest()
	breq.Page = pageBuildJSON
	verifyPath := breq.urlPath()

	verify := func(verifierBaseURL string) (*buildJSON, error) {
		t0 := time.Now()
		defer func() {
			metricVerifyDuration.WithLabelValues(verifierBaseURL, req.Goos, req.Goarch, req.Goversion).Observe(time.Since(t0).Seconds())
		}()

		verifyURL := verifierBaseURL + verifyPath
		resp, err := http.Get(verifyURL)
		if err != nil {
			return nil, fmt.Errorf("%w: http request: %v", errServer, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			metricVerifyErrors.WithLabelValues(verifierBaseURL, req.Goos, req.Goarch, req.Goversion).Inc()
			buf, err := ioutil.ReadAll(resp.Body)
			msg := string(buf)
			if err != nil {
				msg = fmt.Sprintf("reading error message: %v", err)
			}
			return nil, fmt.Errorf("%w: http error response: %s:\n%s", errRemote, resp.Status, msg)
		}
		var result buildJSON
		err = json.NewDecoder(resp.Body).Decode(&result)
		if err != nil {
			return nil, fmt.Errorf("parsing build.json: %v", err)
		}
		return &result, nil
	}

	for _, verifierBaseURL := range config.VerifierURLs {
		go func(verifierBaseURL string) {
			result, err := verify(verifierBaseURL)
			if err != nil {
				err = fmt.Errorf("verifying with %s: %w", verifierBaseURL, err)
			}
			verifyResult <- remoteBuild{verifierBaseURL, err, result}
		}(verifierBaseURL)
	}

	t0 := time.Now()

	lname := dir + "/bin/" + req.filename()
	os.Mkdir(filepath.Dir(lname), 0775) // failures will be caught later
	cmd := exec.Command(gobin, "build", "-mod=readonly", "-o", lname, "-x", "-v", "-trimpath", "-ldflags=-buildid=")
	cmd.Env = []string{
		fmt.Sprintf("GOPROXY=%s", config.GoProxy),
		"GO111MODULE=on",
		"GO19CONCURRENTCOMPILATION=0",
		"CGO_ENABLED=0",
		"GOOS=" + req.Goos,
		"GOARCH=" + req.Goarch,
		"HOME=" + homedir,
	}
	cmd.Dir = pkgDir
	output, err := cmd.CombinedOutput()
	var sysTime, userTime time.Duration
	if cmd.ProcessState != nil {
		sysTime = cmd.ProcessState.SystemTime()
		userTime = cmd.ProcessState.UserTime()
	}
	metricCompileDuration.WithLabelValues(req.Goos, req.Goarch, req.Goversion).Observe(time.Since(t0).Seconds())
	if err != nil {
		metricCompileErrors.WithLabelValues(req.Goos, req.Goarch, req.Goversion).Inc()
		err := saveFailure(req, err.Error()+"\n\n"+string(output), start, sysTime, userTime)
		if err != nil {
			return nil, fmt.Errorf("storing results of failure: %v (%w)", err, errTempFailure)
		}
		return nil, err
	}

	elapsed := time.Since(start)

	var tmpdir string
	result, tmpdir, err = saveFiles(req, output, lname, start, elapsed, sysTime, userTime)
	defer func() {
		if tmpdir != "" {
			os.RemoveAll(tmpdir)
		}
	}()
	if err != nil {
		return nil, fmt.Errorf("storing results of build: %v (%w)", err, errTempFailure)
	}

	matchesFrom := []string{}
	mismatches := []string{}
	for n := len(config.VerifierURLs); n > 0; n-- {
		vr := <-verifyResult
		if vr.err != nil {
			return nil, fmt.Errorf("build at verifier failed: %v (%w)", vr.err, errTempFailure)
		}
		if result.Sum == vr.result.Sum {
			matchesFrom = append(matchesFrom, vr.verifyURL)
		} else {
			mismatches = append(mismatches, fmt.Sprintf("%s got %s", vr.verifyURL, vr.result.Sum))
		}
	}
	if len(mismatches) > 0 {
		return nil, fmt.Errorf("build mismatches, we and %d others got %s, but %s (%w)", len(matchesFrom), result.Sum, strings.Join(mismatches, ", "), errTempFailure)
	}

	finalDir := path.Join(config.DataDir, req.storeDir())
	os.MkdirAll(path.Dir(finalDir), 0775) // failures will be caught later
	err = os.Rename(tmpdir, finalDir)
	if err != nil {
		return nil, err
	}
	tmpdir = ""

	err = appendBuildsTxt(result)
	if err != nil {
		return nil, err
	}
	return result, nil
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

	finalDir := path.Join(config.DataDir, req.storeDir())
	os.MkdirAll(path.Dir(finalDir), 0775) // failures will be caught later
	err = os.Rename(tmpdir, finalDir)
	if err != nil {
		return err
	}
	tmpdir = ""

	buildResult := &buildJSON{
		"v0",
		"",
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

func saveFiles(req request, output []byte, lname string, start time.Time, elapsed, systemTime, userTime time.Duration) (*buildJSON, string, error) {
	of, err := os.Open(lname)
	if err != nil {
		return nil, "", err
	}
	defer of.Close()

	fi, err := of.Stat()
	if err != nil {
		return nil, "", err
	}
	size := fi.Size()

	h := sha256.New()
	_, err = io.Copy(h, of)
	if err != nil {
		return nil, "", err
	}
	sha256 := h.Sum(nil)
	_, err = of.Seek(0, io.SeekStart)
	if err != nil {
		return nil, "", err
	}

	tmpdir, err := ioutil.TempDir(config.DataDir, "success")
	if err != nil {
		return nil, "", err
	}
	// note: tmpdir is removed by caller!

	buildResult := buildJSON{
		"v0",
		"0" + base64.RawURLEncoding.EncodeToString(sha256[:20]),
		sha256,
		size,
		0, // filled in below
		start,
		elapsed,
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
		return nil, tmpdir, err
	}

	binGz := tmpdir + "/" + req.downloadFilename() + ".gz"
	err = writeGz(binGz, of)
	if err != nil {
		return nil, tmpdir, err
	}
	fi, err = os.Stat(binGz)
	if err != nil {
		return nil, tmpdir, err
	}
	buildResult.FilesizeGz = fi.Size()

	buf, err := json.Marshal(buildResult)
	if err != nil {
		return nil, tmpdir, fmt.Errorf("%w: marshal build.json: %v", errServer, err)
	}
	err = ioutil.WriteFile(tmpdir+"/build.json", buf, 0664)
	if err != nil {
		return nil, tmpdir, err
	}
	return &buildResult, tmpdir, nil
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

func appendBuildsTxt(b *buildJSON) error {
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
