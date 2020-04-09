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
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var errTempFailure = errors.New("temporary failure")

type buildJSON struct {
	V             string // "0"
	Sum           string // Sum is the versioned raw-base64-url encoded 20-byte prefix of the SHA256 sum. For v 0, it starts with "0".
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
	RecordNumber  int64
}

func ensureGobin(req request) (string, error) {
	gobin := filepath.Join(config.SDKDir, req.Goversion, "bin", "go"+goexe())
	if !filepath.IsAbs(gobin) {
		gobin = filepath.Join(workdir, gobin)
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
		return fmt.Errorf("missing toolchain %q: %w", req.Goversion, err)
	}

	gobin, err := ensureGobin(req)
	if err != nil {
		return err
	}

	modDir, getOutput, err := ensureModule(gobin, req.Mod, req.Version)
	if err != nil {
		return fmt.Errorf("error fetching module from goproxy: %w\n\n# output from go get:\n%s", err, string(getOutput))
	}

	pkgDir := filepath.Join(modDir, filepath.FromSlash(req.Dir))

	// Check if package is a main package, resulting in an executable when built.
	cgo := true
	cmd := makeCommand(cgo, gobin, "list", "-f", "{{.Name}}")
	cmd.Dir = pkgDir
	cmd.Env = append(cmd.Env,
		"GOOS="+req.Goos,
		"GOARCH="+req.Goarch,
	)
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	nameOutput, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("error finding package name; perhaps package does not exist: %v\n\n# stdout from go list:\n%s\n\nstderr:\n%s", err, nameOutput, stderr.String())
	}
	if string(nameOutput) != "main\n" {
		return fmt.Errorf("package main %w, building would not result in executable binary (package %s)", errNotExist, strings.TrimRight(string(nameOutput), "\n"))
	}

	// Check that package does not depend on any cgo.
	cmd = makeCommand(cgo, gobin, "list", "-deps", "-f", `{{ if and (not .Standard) .CgoFiles }}{{ .ImportPath }}{{ end }}`)
	cmd.Dir = pkgDir
	cmd.Env = append(cmd.Env,
		"GOOS="+req.Goos,
		"GOARCH="+req.Goarch,
	)
	stderr = &strings.Builder{}
	cmd.Stderr = stderr
	cgoOutput, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("error determining whether cgo is required: %v\n\n# output from go list:\n%s\n\nstderr:\n%s", err, cgoOutput, stderr.String())
	}
	if len(cgoOutput) != 0 {
		return fmt.Errorf("build %w due to cgo dependencies:\n\n%s", errNotExist, cgoOutput)
	}
	return nil
}

// goBuild performs the actual build. This is called from coordinate.go, not too
// many at a time.
func goBuild(req request) (*buildJSON, error) {
	bu := req.buildIndexRequest().urlPath()

	result, err := build(req)

	ok := err == nil && result != nil
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
	targets.increase(req.Goos + "/" + req.Goos)

	start := time.Now()

	gobin, err := ensureGobin(req)
	if err != nil {
		return nil, fmt.Errorf("ensuring go version is available: %v (%w)", err, errTempFailure)
	}

	_, getOutput, err := ensureModule(gobin, req.Mod, req.Version)
	if err != nil {
		return nil, fmt.Errorf("error fetching module from goproxy: %v (%w)\n\n# output from go get:\n%s", err, errTempFailure, string(getOutput))
	}

	dir, err := ioutil.TempDir("", "gobuild")
	if err != nil {
		return nil, fmt.Errorf("tempdir for build: %v (%w)", err, errTempFailure)
	}
	defer os.RemoveAll(dir)

	// Launch goroutines to let them build the same code and return their build.json.
	// After our build, we verify we all had the same result. If our build fails, we
	// just ignore these results, and let the remote builds continue. They will not
	// cancel the build anyway.

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

	// What to "go get".
	name := req.Mod
	if req.Dir != "" {
		name += "/" + req.Dir
	}
	name += "@" + req.Version

	// Path to compiled binary written by go get. We need to use "go get" to get full
	// module version information in the binary. That isn't possible with "go build".
	// But only "go build" has an "-o" flag to specify the output. And "go get" won't
	// build with $GOBIN set.
	resultPath := filepath.Join(homedir, "go", "bin")
	if req.Goos != runtime.GOOS || req.Goarch != runtime.GOARCH {
		resultPath = filepath.Join(resultPath, req.Goos+"_"+req.Goarch)
	}
	if req.Dir == "" {
		resultPath = filepath.Join(resultPath, filepath.Base(req.Mod))
	} else {
		resultPath = filepath.Join(resultPath, filepath.Base(req.Dir))
	}
	// Also cannot set "GOEXE", "go get" does not use it.
	if req.Goos == "windows" {
		resultPath += ".exe"
	}

	// Ensure the file does not exist before trying to create it.
	// This might be a leftover from some earlier build attempt.
	err = os.Remove(resultPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("attempting to remove preexisting binary: %v (%w)", err, errTempFailure)
	}

	// Always remove binary from $GOBIN when we're done here. We copied it on success.
	defer func() {
		os.Remove(resultPath)
	}()

	cgo := false
	cmd := makeCommand(cgo, gobin, "get", "-x", "-v", "-trimpath", "-ldflags=-buildid=", "--", name)
	cmd.Env = append(cmd.Env,
		"GOOS="+req.Goos,
		"GOARCH="+req.Goarch,
	)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	var sysTime, userTime time.Duration
	if cmd.ProcessState != nil {
		sysTime = cmd.ProcessState.SystemTime()
		userTime = cmd.ProcessState.UserTime()
	}
	metricCompileDuration.WithLabelValues(req.Goos, req.Goarch, req.Goversion).Observe(time.Since(t0).Seconds())
	if err != nil {
		metricCompileErrors.WithLabelValues(req.Goos, req.Goarch, req.Goversion).Inc()
		err2 := saveFailure(req, err.Error()+"\n\n"+string(output), start, sysTime, userTime)
		if err2 != nil {
			return nil, fmt.Errorf("storing results of failure: %v (%w)", err2, errTempFailure)
		}
		return nil, err
	}

	elapsed := time.Since(start)

	tmpdir, err := ioutil.TempDir(config.DataDir, "tmpgobuild")
	if err != nil {
		return nil, err
	}
	defer func() {
		if tmpdir != "" {
			os.RemoveAll(tmpdir)
		}
	}()

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

	result, err = saveFiles(tmpdir, req, output, resultPath, start, elapsed, sysTime, userTime)
	if err != nil {
		return nil, fmt.Errorf("storing results of build: %v (%w)", err, errTempFailure)
	}

	finalDir := filepath.Join(config.DataDir, "result", req.storeDir())
	os.MkdirAll(filepath.Dir(finalDir), 0775) // failures will be caught later
	err = os.Rename(tmpdir, finalDir)
	if err != nil {
		return nil, err
	}
	tmpdir = ""

	return result, nil
}

func saveFailure(req request, output string, start time.Time, systemTime, userTime time.Duration) error {
	tmpdir, err := ioutil.TempDir(config.DataDir, "tmpfailure")
	if err != nil {
		return err
	}
	defer func() {
		if tmpdir != "" {
			os.RemoveAll(tmpdir)
		}
	}()

	err = writeGz(filepath.Join(tmpdir, "log.gz"), strings.NewReader(output))
	if err != nil {
		return err
	}

	finalDir := filepath.Join(config.DataDir, "result", req.storeDir())
	os.MkdirAll(filepath.Dir(finalDir), 0775) // failures will be caught later
	err = os.Rename(tmpdir, finalDir)
	if err != nil {
		return err
	}
	tmpdir = ""
	return nil
}

func saveFiles(tmpdir string, req request, output []byte, resultPath string, start time.Time, elapsed, systemTime, userTime time.Duration) (*buildJSON, error) {
	of, err := os.Open(resultPath)
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

	buildResult := buildJSON{
		"0",
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
		-1, // filled in by addSum below
	}

	if err := addSum(tmpdir, &buildResult); err != nil {
		return nil, fmt.Errorf("adding sum to tranparency log: %w", err)
	}

	err = writeGz(filepath.Join(tmpdir, "log.gz"), bytes.NewReader(output))
	if err != nil {
		return nil, err
	}

	binGz := filepath.Join(tmpdir, req.downloadFilename()+".gz")
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
		return nil, fmt.Errorf("%w: marshal build.json: %v", errServer, err)
	}
	err = ioutil.WriteFile(filepath.Join(tmpdir, "build.json"), buf, 0664)
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
