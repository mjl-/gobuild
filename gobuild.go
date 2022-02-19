package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var errTempFailure = errors.New("temporary failure")

func ensureGobin(goversion string) (string, error) {
	gobin := filepath.Join(config.SDKDir, goversion, "bin", "go"+goexe())
	if !filepath.IsAbs(gobin) {
		gobin = filepath.Join(workdir, gobin)
	}
	if _, err := os.Stat(gobin); err != nil {
		return "", fmt.Errorf("unknown toolchain %q: %v", goversion, err)
	}
	return gobin, nil
}

func prepareBuild(bs buildSpec) error {
	if err := ensureSDK(bs.Goversion); err != nil {
		return fmt.Errorf("missing toolchain %q: %w", bs.Goversion, err)
	}

	gobin, err := ensureGobin(bs.Goversion)
	if err != nil {
		return err
	}

	modDir, getOutput, err := ensureModule(bs.Goversion, gobin, bs.Mod, bs.Version)
	if err != nil {
		return fmt.Errorf("error fetching module from goproxy: %w\n\n# output from go get:\n%s", err, string(getOutput))
	}

	pkgDir := filepath.Join(modDir, filepath.FromSlash(bs.Dir[1:]))

	// Check if package is a main package, resulting in an executable when built.
	goproxy := true
	cgo := true
	moreEnv := []string{
		"GOOS=" + bs.Goos,
		"GOARCH=" + bs.Goarch,
	}
	cmd := makeCommand(goproxy, pkgDir, cgo, moreEnv, gobin, "list", "-f", "{{.Name}}")
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	if nameOutput, err := cmd.Output(); err != nil {
		return fmt.Errorf("error finding package name; perhaps package does not exist: %v\n\n# stdout from go list:\n%s\n\nstderr:\n%s", err, nameOutput, stderr.String())
	} else if string(nameOutput) != "main\n" {
		return fmt.Errorf("package main %w, building would not result in executable binary (package %s)", errNotExist, strings.TrimRight(string(nameOutput), "\n"))
	}

	// Check that package does not depend on any cgo.
	cmd = makeCommand(goproxy, pkgDir, cgo, moreEnv, gobin, "list", "-mod=mod", "-deps", "-f", `{{ if and (not .Standard) .CgoFiles }}{{ .ImportPath }}{{ end }}`)
	stderr = &strings.Builder{}
	cmd.Stderr = stderr
	if cgoOutput, err := cmd.Output(); err != nil {
		return fmt.Errorf("error determining whether cgo is required: %v\n\n# output from go list:\n%s\n\nstderr:\n%s", err, cgoOutput, stderr.String())
	} else if len(cgoOutput) != 0 {
		return fmt.Errorf("build %w due to cgo dependencies:\n\n%s", errNotExist, cgoOutput)
	}
	return nil
}

// Build does the actual build. It is called from coordinate, ensuring the same
// buildSpec isn't built multiple times concurrently, and preventing a few other
// clashes. On success, a record has been added to the transparency log.
func build(bs buildSpec) (int64, *buildResult, error) {
	targets.increase(bs.Goos + "/" + bs.Goarch)

	gobin, err := ensureGobin(bs.Goversion)
	if err != nil {
		return -1, nil, fmt.Errorf("ensuring go version is available: %v (%w)", err, errTempFailure)
	}

	if _, output, err := ensureModule(bs.Goversion, gobin, bs.Mod, bs.Version); err != nil {
		return -1, nil, fmt.Errorf("error fetching module from goproxy: %v (%w)\n\n# output from go get:\n%s", err, errTempFailure, output)
	}

	// Launch goroutines to let the verifiers build the same code and return their
	// build result. After our build, we verify we all had the same result. If our
	// build fails, we just ignore these results, and let the remote builds continue.
	// They will not cancel the build anyway.

	type remoteBuild struct {
		verifyURL string
		err       error
		result    *buildResult
	}

	verifyResult := make(chan remoteBuild, len(config.VerifierURLs))
	verifyLink := request{bs, "", pageRecord}.link()

	verify := func(verifierBaseURL string) (*buildResult, error) {
		t0 := time.Now()
		defer func() {
			metricVerifyDuration.WithLabelValues(verifierBaseURL, bs.Goos, bs.Goarch, bs.Goversion).Observe(time.Since(t0).Seconds())
		}()

		verifyURL := verifierBaseURL + verifyLink
		resp, err := httpGet(verifyURL)
		if err != nil {
			return nil, fmt.Errorf("%w: http request: %v", errServer, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			metricVerifyErrors.WithLabelValues(verifierBaseURL, bs.Goos, bs.Goarch, bs.Goversion).Inc()
			buf, err := ioutil.ReadAll(resp.Body)
			msg := string(buf)
			if err != nil {
				msg = fmt.Sprintf("reading error message: %v", err)
			}
			return nil, fmt.Errorf("%w: http error response: %s:\n%s", errRemote, resp.Status, msg)
		}

		if msg, err := ioutil.ReadAll(resp.Body); err != nil {
			return nil, fmt.Errorf("reading build result from remote: %v", err)
		} else if br, err := parseRecord(msg); err != nil {
			return nil, fmt.Errorf("parsing build record from remote: %v", err)
		} else {
			return br, nil
		}
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

	if err := ensurePrimedBuildCache(gobin, bs.Goos, bs.Goarch, bs.Goversion); err != nil {
		return -1, nil, fmt.Errorf("%w: ensuring primed go build cache: %v", errServer, err)
	}

	// What to "go get".
	name := bs.Mod
	if bs.Dir != "/" {
		name += bs.Dir
	}
	name += "@" + bs.Version

	// Path to compiled binary written by go get. We need to use "go get" to get full
	// module version information in the binary. That isn't possible with "go build".
	// But only "go build" has an "-o" flag to specify the output. And "go get" won't
	// build with $GOBIN set.
	var resultPath string
	if bs.Dir != "/" {
		resultPath = filepath.Join(resultPath, filepath.Base(bs.Dir[1:]))
	} else {
		resultPath = filepath.Join(resultPath, filepath.Base(bs.Mod))
	}
	// Also cannot set "GOEXE", "go get" does not use it.
	if bs.Goos == "windows" {
		resultPath += ".exe"
	}
	if bs.Goos != runtime.GOOS || bs.Goarch != runtime.GOARCH {
		resultPath = filepath.Join(bs.Goos+"_"+bs.Goarch, resultPath)
	}

	moreEnv := []string{
		"GOOS=" + bs.Goos,
		"GOARCH=" + bs.Goarch,
	}

	var gobuildbindir string
	if config.BuildGobin {
		// Require build command (through config.Run) to write the target binary to a
		// tempdir which we'll pass through GOBUILD_GOBIN. The build command can make only
		// that directory writable, and with this temp dir it will never clash with other
		// builds.
		gobuildbindir, err = ioutil.TempDir("", "gobuildbindir")
		if err != nil {
			return -1, nil, fmt.Errorf("making temp dir: %v", err)
		}
		moreEnv = append(moreEnv, "GOBUILD_GOBIN="+gobuildbindir)
		resultPath = filepath.Join(gobuildbindir, resultPath)
		defer os.RemoveAll(gobuildbindir)
	} else {
		resultPath = filepath.Join(homedir, "go", "bin", resultPath)
	}

	// Ensure the file does not exist before trying to create it.
	// This might be a leftover from some earlier build attempt.
	err = os.Remove(resultPath)
	if err != nil && !os.IsNotExist(err) {
		return -1, nil, fmt.Errorf("attempting to remove preexisting binary: %v (%w)", err, errTempFailure)
	}

	// Always remove binary from $GOBIN when we're done here. We copied it on success.
	defer os.Remove(resultPath)

	goproxy := false
	cgo := false
	var cmd *exec.Cmd
	if version1, ok := goversion1(bs.Goversion); ok && version1 >= 18 {
		// Since Go1.18 we need to use "go install" to compile external programs.
		cmd = makeCommand(goproxy, emptyDir, cgo, moreEnv, gobin, "install", "-x", "-v", "-trimpath", "-ldflags=-buildid=", "--", name)
	} else {
		cmd = makeCommand(goproxy, emptyDir, cgo, moreEnv, gobin, "get", "-x", "-v", "-trimpath", "-ldflags=-buildid=", "--", name)
	}
	output, err := cmd.CombinedOutput()
	metricCompileDuration.WithLabelValues(bs.Goos, bs.Goarch, bs.Goversion).Observe(time.Since(t0).Seconds())
	if err != nil {
		metricCompileErrors.WithLabelValues(bs.Goos, bs.Goarch, bs.Goversion).Inc()
		if xerr := saveFailure(bs, err.Error()+"\n\n"+string(output)); xerr != nil {
			return -1, nil, fmt.Errorf("storing results of failure: %v (%w)", xerr, errTempFailure)
		}
		return -1, nil, err
	}

	// Where we store the "recordnumber" file, binary.gz and log.gz.
	tmpdir, err := ioutil.TempDir(resultDir, "tmpresult")
	if err != nil {
		return -1, nil, err
	}
	// On success, the directory will have been moved to its final destination,
	// indicated by an empty tmpdir.
	defer func() {
		if tmpdir != "" {
			os.RemoveAll(tmpdir)
		}
	}()

	br := buildResult{buildSpec: bs}

	// Calculate our hash.
	rf, err := os.Open(resultPath)
	if err != nil {
		return -1, nil, fmt.Errorf("open result: %v", err)
	}
	defer rf.Close()

	if info, err := rf.Stat(); err != nil {
		return -1, nil, fmt.Errorf("stat result: %v", err)
	} else {
		br.Filesize = info.Size()
	}

	h := sha256.New()
	if _, err := io.Copy(h, rf); err != nil {
		return -1, nil, fmt.Errorf("read result: %v", err)
	} else if _, err := rf.Seek(0, 0); err != nil {
		return -1, nil, fmt.Errorf("seek result: %v", err)
	}
	br.Sum = "0" + base64.RawURLEncoding.EncodeToString(h.Sum(nil)[:20])

	// Verify the sums of the verifiers.
	matchesFrom := []string{}
	mismatches := []string{}
	for n := len(config.VerifierURLs); n > 0; n-- {
		vr := <-verifyResult
		if vr.err != nil {
			return -1, nil, fmt.Errorf("build at verifier failed: %v (%w)", vr.err, errTempFailure)
		}
		if vr.result.Sum == br.Sum {
			matchesFrom = append(matchesFrom, vr.verifyURL)
		} else {
			mismatches = append(mismatches, fmt.Sprintf("%s got %s", vr.verifyURL, vr.result.Sum))
		}
	}
	if len(mismatches) > 0 {
		return -1, nil, fmt.Errorf("build mismatches, we and %d others got %s, but %s (%w)", len(matchesFrom), br.Sum, strings.Join(mismatches, ", "), errTempFailure)
	}

	// Write binary and log.
	if err := writeGz(filepath.Join(tmpdir, "binary.gz"), rf); err != nil {
		return -1, nil, err
	}
	if err := writeGz(filepath.Join(tmpdir, "log.gz"), bytes.NewReader(output)); err != nil {
		return -1, nil, err
	}

	// Finally, add to the transparency log, creating the "recordnumber" file and
	// renaming tmpdir to the final directory in resultDir.
	recordNumber, err := addSum(tmpdir, br)
	if err != nil {
		return -1, nil, fmt.Errorf("adding sum to tranparency log: %w", err)
	}
	tmpdir = ""

	recentBuilds.Lock()
	recentBuilds.links = append(recentBuilds.links, request{bs, br.Sum, pageIndex}.link())
	if len(recentBuilds.links) > 10 {
		recentBuilds.links = recentBuilds.links[len(recentBuilds.links)-10:]
	}
	recentBuilds.Unlock()

	return recordNumber, &br, nil
}

func saveFailure(bs buildSpec, output string) error {
	tmpdir, err := ioutil.TempDir(resultDir, "tmpfail")
	if err != nil {
		return err
	}
	defer func() {
		if tmpdir != "" {
			os.RemoveAll(tmpdir)
		}
	}()

	if err := writeGz(filepath.Join(tmpdir, "log.gz"), strings.NewReader(output)); err != nil {
		return err
	}

	if err := os.Rename(tmpdir, bs.storeDir()); err != nil {
		return err
	}
	tmpdir = ""
	return nil
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
	if _, err := io.Copy(lfgz, src); err != nil {
		return err
	}
	if err := lfgz.Close(); err != nil {
		return err
	}
	err = lf.Close()
	lf = nil
	return err
}
