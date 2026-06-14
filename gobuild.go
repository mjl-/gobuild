package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// errTempFailure indicates a build failure has some temporary/transient reason,
// and should be retried again. Caller should also check for context.Canceled
// because builds can be canceled by a shutdown signal.
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

func prepareBuild(ctx context.Context, bs buildSpec) error {
	if _, err := ensureSDK(ctx, bs.Goversion); err != nil {
		return fmt.Errorf("ensuring toolchain %q: %w", bs.Goversion, err)
	}

	gobin, err := ensureGobin(bs.Goversion)
	if err != nil {
		return err
	}

	cmdDir, err := newCommandDir(ctx, "preparebuild")
	if err != nil {
		return err
	}
	defer removeCommandDir(ctx, cmdDir)

	modDir, getOutput, err := ensureModule(ctx, cmdDir, bs.Goversion, gobin, bs.Mod, bs.Version)
	if err != nil {
		return fmt.Errorf("error fetching module from goproxy for preparation: %w\n\n# output from go get:\n%s", err, string(getOutput))
	}

	pkgDir := filepath.Join(modDir, filepath.FromSlash(bs.Dir[1:]))

	if release, err := commandAcquire(ctx); err != nil {
		return err
	} else {
		defer release()
	}

	// Check if package is a main package, resulting in an executable when built.
	goproxy := true
	cgo := true
	moreEnv := []string{
		"GOOS=" + bs.Goos,
		"GOARCH=" + bs.Goarch,
	}
	cmd := makeCommand(ctx, cmdDir, pkgDir, bs.Goversion, goproxy, cgo, moreEnv, gobin, "list", "-f", "{{.Name}}")
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	if nameOutput, err := cmd.Output(); err != nil {
		metricListPackageErrors.Inc()
		return fmt.Errorf("error finding package name; perhaps package does not exist: %v\n\n# stdout from go list:\n%s\n\nstderr:\n%s", err, nameOutput, stderr.String())
	} else if string(nameOutput) != "main\n" {
		metricNotMainErrors.Inc()
		return fmt.Errorf("package main %w, building would not result in executable binary (package %s)", errNotExist, strings.TrimRight(string(nameOutput), "\n"))
	}

	return nil
}

// remoteBuild is the result of a build done by a remote server, for verification.
type remoteBuild struct {
	// Either verifier or verifyURL is set, depending on config.Verifiers (new) or
	// config.VerifierURL (old).
	verifier  *Verifier
	verifyURL string

	err    error
	result *buildResult
}

// name a string to use in logging to identify a verifier or verifierURL.
func (rb *remoteBuild) name() string {
	if rb.verifier != nil {
		return rb.verifier.name
	}
	return rb.verifyURL
}

// Build does the actual build. It is called from coordinate, ensuring the same
// buildSpec isn't built multiple times concurrently, and preventing a few other
// clashes.
//
// If expSumOpt is non-empty, a build was done in the past but the binary removed.
// This build will restore the binary. If expSumOpt is empty and the build is
// successful, a record is added to the transparency log.
//
// On success, returns a record number and build result.
// On failure, returns an error (last value), and optionally the output as
// string followed by a reason in case this build won't succeed in the future
// (e.g. due to invalid code and/or compiler combination).
func build(ctx context.Context, bs buildSpec, expSumOpt string) (int64, *buildResult, string, string, error) {
	targets.increase(bs.Goos + "/" + bs.Goarch)

	gobin, err := ensureGobin(bs.Goversion)
	if err != nil {
		return -1, nil, "", "", fmt.Errorf("ensuring go version is available: %v (%w)", err, errTempFailure)
	}

	cmdDir, err := newCommandDir(ctx, "build")
	if err != nil {
		return -1, nil, "", "", fmt.Errorf("creating temporary command directory: %v (%w)", err, errTempFailure)
	}
	defer removeCommandDir(ctx, cmdDir)

	if _, output, err := ensureModule(ctx, cmdDir, bs.Goversion, gobin, bs.Mod, bs.Version); err != nil {
		return -1, nil, "", "", fmt.Errorf("error fetching module from goproxy for build: %v (%w)\n\n# output from go get:\n%s", err, errTempFailure, output)
	}

	// Launch goroutines to let the verifiers build the same code and return their
	// build result. After our build, we verify we all had the same result. If our
	// build fails, we just ignore these results, and let the remote builds continue.
	// They will not cancel the build anyway.

	verifyResult := make(chan remoteBuild, len(config.Verifiers)+len(config.VerifierURLs))
	verifyLink := request{bs, "", pageRecord}.link()

	// Verify a build at remote based on Verifier, which looks up a build in the
	// remote's transparency log.
	verify := func(v Verifier) (*buildResult, error) {
		t0 := time.Now()
		defer func() {
			metricVerifyDuration.WithLabelValues(v.name).Observe(time.Since(t0).Seconds())
		}()

		key := bs.String()
		_, data, err := v.client.Lookup(ctx, key)
		if err != nil {
			metricVerifyErrors.WithLabelValues(v.name).Inc()
			return nil, fmt.Errorf("%w: looking up build in remote transparency log: %v", errRemote, err)
		}

		br, err := parseRecord(data)
		if err != nil {
			metricVerifyErrors.WithLabelValues(v.name).Inc()
			return nil, fmt.Errorf("parsing build record from remote: %v", err)
		}
		return br, nil
	}

	// verifyURL is like verify above, but older and less desirable because it doesn't
	// look up the result in the remote's transparency log.
	verifyURL := func(verifierBaseURL string) (*buildResult, error) {
		t0 := time.Now()
		defer func() {
			metricVerifyDuration.WithLabelValues(verifierBaseURL).Observe(time.Since(t0).Seconds())
		}()

		verifyURL := verifierBaseURL + verifyLink
		resp, err := httpGet(ctx, verifyURL)
		if err != nil {
			return nil, fmt.Errorf("%w: http request: %v", errServer, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			metricVerifyErrors.WithLabelValues(verifierBaseURL).Inc()
			buf, err := io.ReadAll(resp.Body)
			msg := string(buf)
			if err != nil {
				msg = fmt.Sprintf("reading error message: %v", err)
			}
			return nil, fmt.Errorf("%w: http error response: %s:\n%s", errRemote, resp.Status, msg)
		}

		if msg, err := io.ReadAll(resp.Body); err != nil {
			return nil, fmt.Errorf("reading build result from remote: %v", err)
		} else if br, err := parseRecord(msg); err != nil {
			return nil, fmt.Errorf("parsing build record from remote: %v", err)
		} else {
			return br, nil
		}
	}

	for _, v := range config.Verifiers {
		wgShutdown.Go(func() {
			defer logPanic(logger(ctx))

			result, err := verify(v)
			if err != nil {
				err = fmt.Errorf("verifying with verifier %s: %w", v.Key, err)
			}
			verifyResult <- remoteBuild{&v, "", err, result}
		})
	}

	for _, verifierBaseURL := range config.VerifierURLs {
		wgShutdown.Go(func() {
			defer logPanic(logger(ctx))

			result, err := verifyURL(verifierBaseURL)
			if err != nil {
				err = fmt.Errorf("verifying with verifierURL %s: %w", verifierBaseURL, err)
			}
			verifyResult <- remoteBuild{nil, verifierBaseURL, err, result}
		})
	}

	t0 := time.Now()

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
		gobuildbindir, err = os.MkdirTemp("", "gobuildbindir")
		if err != nil {
			return -1, nil, "", "", fmt.Errorf("making temp dir: %v", err)
		}
		moreEnv = append(moreEnv, "GOBUILD_GOBIN="+gobuildbindir)
		resultPath = filepath.Join(gobuildbindir, resultPath)
		defer func() {
			if err := os.RemoveAll(gobuildbindir); err != nil {
				logger(ctx).Error("removing temporary gobuildbindir", "err", err, "dir", gobuildbindir)
			}
		}()
	} else {
		resultPath = filepath.Join(cmdDir, "go", "bin", resultPath)
	}

	// Ensure the file does not exist before trying to create it.
	// This might be a leftover from some earlier build attempt.
	err = os.Remove(resultPath)
	if err != nil && !os.IsNotExist(err) {
		return -1, nil, "", "", fmt.Errorf("attempting to remove preexisting binary: %v (%w)", err, errTempFailure)
	}

	// Always remove binary from $GOBIN when we're done here. We copied it on success.
	defer os.Remove(resultPath)

	// We strip out the buildid. The first of the 4 slash-separated parts will vary
	// with different setups (toolchains on different systems and/or their installation
	// location). We hash the whole binary, and it must be the same regardless of
	// system it was compiled on. Perhaps we should just clear out the first part,
	// keeping the remaining parts. Some (or all?) of those parts are content hashes.
	// Could be helpful for debugging. NOTE: before go1.13.3, working directories of
	// builds would affect the resulting binary.

	goproxy := false
	cgo := false
	var cmd *exec.Cmd
	gv, err := parseGoVersion(bs.Goversion)
	if err != nil {
		return -1, nil, "", "", fmt.Errorf("%w: %s", errBadGoversion, err)
	}
	ldflags := "-buildid="
	if bs.Stripped {
		ldflags += " -s"
	}
	// Since Go1.18 we need to use "go install" to compile external programs.
	if gv.major == 1 && gv.minor >= 18 {
		// Go1.23 started checking for deprecations during "go install", requiring GOPROXY
		// access. https://golang.org/cl/528775
		if gv.major == 1 && gv.minor >= 23 {
			goproxy = true
		}
		cmd = makeCommand(ctx, cmdDir, cmdDir, bs.Goversion, goproxy, cgo, moreEnv, gobin, "install", "-x", "-v", "-trimpath", "-ldflags="+ldflags, "--", name)
	} else {
		cmd = makeCommand(ctx, cmdDir, cmdDir, bs.Goversion, goproxy, cgo, moreEnv, gobin, "get", "-x", "-v", "-trimpath", "-ldflags="+ldflags, "--", name)
	}
	output, err := cmd.CombinedOutput()
	metricCompileDuration.Observe(time.Since(t0).Seconds())
	if err != nil {
		metricCompileOSErrors.WithLabelValues(bs.Goos).Inc()
		metricCompileArchErrors.WithLabelValues(bs.Goarch).Inc()
		metricCompileVersionErrors.WithLabelValues(bs.Goversion).Inc()
		out := string(output)
		reason := cannotBuild(out)
		if xerr := saveFailure(ctx, bs, err, out, reason); xerr != nil {
			return -1, nil, "", reason, fmt.Errorf("storing results of failure: %v (%w)", xerr, errTempFailure)
		}
		return -1, nil, out, reason, err
	}

	// Where we store the "recordnumber" file, binary.gz and log.gz.
	tmpdir, err := os.MkdirTemp(config.DataDir, "tmpresult")
	if err != nil {
		return -1, nil, "", "", err
	}
	// On success, the directory will have been moved to its final destination,
	// indicated by an empty tmpdir.
	defer func() {
		if tmpdir != "" {
			if err := os.RemoveAll(tmpdir); err != nil {
				logger(ctx).Error("remove tmpresult dir", "err", err, "tmpdir", tmpdir)
			}
		}
	}()

	br := buildResult{buildSpec: bs}

	// Calculate our hash.
	rf, err := os.Open(resultPath)
	if err != nil {
		return -1, nil, "", "", fmt.Errorf("open result: %v", err)
	}
	defer rf.Close()

	if info, err := rf.Stat(); err != nil {
		return -1, nil, "", "", fmt.Errorf("stat result: %v", err)
	} else {
		br.Filesize = info.Size()
	}

	h := sha256.New()
	if _, err := io.Copy(h, rf); err != nil {
		return -1, nil, "", "", fmt.Errorf("read result: %v", err)
	} else if _, err := rf.Seek(0, 0); err != nil {
		return -1, nil, "", "", fmt.Errorf("seek result: %v", err)
	}
	br.Sum = "0" + base64.RawURLEncoding.EncodeToString(h.Sum(nil)[:20])

	// Called after a binary.gz is added to the result/ data directory. Either when
	// recreating the binary, or when first creating it.
	handleBinaryCacheAdd := func(size int64) {
		// Cache size administration isn't entirely race-free. Multiple processes may
		// finish a build and a positive number of bytes to reclaim. Perhaps a little too
		// many or too few files will get cleaned up. The administration of
		// result/binary-cache-size.txt should be correct though and files would get
		// cleaned up during a future build.
		reclaim := binaryCacheSizeAdd(size)
		if err := binaryCacheSizeWrite(); err != nil {
			logger(ctx).Error("writing result/binary-cache-size.txt", "err", err)
			// continuing...
		}
		if reclaim > 0 {
			// Do cleanup in the background, it may take a while with a big cache.
			go func() {
				defer logPanic(logger(ctx))

				if err := binaryCacheCleanup(reclaim); err != nil {
					logger(ctx).Error("cleaning up binary cache for max size", "err", err)
				}
			}()
		}
	}

	// If we already have a sum, we've done this build before and are now restoring the
	// binary. The sum of the newly compiled file must match.
	if expSumOpt != "" {
		if br.Sum != expSumOpt {
			metricRecompileMismatch.WithLabelValues(bs.Goos, bs.Goarch, bs.Goversion).Inc()
			return -1, nil, "", "", fmt.Errorf("sum of rebuilt binary %s does not match previous sum %s", br.Sum, expSumOpt)
		}
		storeDir := br.storeDir()
		ptmp := filepath.Join(tmpdir, "binary.gz")
		pdst := filepath.Join(storeDir, "binary.gz")
		if err := writeGz(ptmp, rf); err != nil {
			return -1, nil, "", "", err
		}
		if recBuf, err := os.ReadFile(filepath.Join(storeDir, "recordnumber")); err != nil {
			return -1, nil, "", "", fmt.Errorf("reading previous recordnumber file: %w", err)
		} else if v, err := strconv.ParseInt(strings.TrimSpace(string(recBuf)), 10, 64); err != nil {
			return -1, nil, "", "", fmt.Errorf("parsing previous recordnumber %q: %v", recBuf, err)
		} else if st, err := os.Stat(ptmp); err != nil {
			return -1, nil, "", "", fmt.Errorf("stat temporary binary.gz for size: %w", err)
		} else if err := os.Rename(ptmp, pdst); err != nil {
			return -1, nil, "", "", fmt.Errorf("moving binary.gz to destination: %w", err)
		} else {
			handleBinaryCacheAdd(st.Size())

			return v, &br, "", "", nil
		}
	}

	// Verify the sums of the verifiers.
	matchesFrom := []string{}
	mismatches := []string{}
	for n := len(config.Verifiers) + len(config.VerifierURLs); n > 0; n-- {
		vr := <-verifyResult
		if vr.err != nil {
			return -1, nil, "", "", fmt.Errorf("build at verifier failed: %v (%w)", vr.err, errTempFailure)
		}
		if vr.result.Sum == br.Sum {
			matchesFrom = append(matchesFrom, vr.name())
		} else {
			metricVerifyMismatch.WithLabelValues(vr.name(), bs.Goos, bs.Goarch, bs.Goversion).Inc()
			logger(ctx).Error("checksum mismatch from verifier", "verifier", vr.name(), "verifiersum", vr.result.Sum, "expectsum", br.Sum)
			mismatches = append(mismatches, fmt.Sprintf("%s got %s", vr.name(), vr.result.Sum))
		}
	}
	if len(mismatches) > 0 {
		if len(matchesFrom) > 0 {
			return -1, nil, "", "", fmt.Errorf("build mismatches, we and %d others got %s, but %s (%w)", len(matchesFrom), br.Sum, strings.Join(mismatches, ", "), errTempFailure)
		}
		return -1, nil, "", "", fmt.Errorf("build mismatches, we got %s, but %s (%w)", br.Sum, strings.Join(mismatches, ", "), errTempFailure)
	}

	// Write binary and log.
	if err := writeGz(filepath.Join(tmpdir, "binary.gz"), rf); err != nil {
		return -1, nil, "", "", err
	}
	if err := writeGz(filepath.Join(tmpdir, "log.gz"), bytes.NewReader(output)); err != nil {
		return -1, nil, "", "", err
	}

	st, err := os.Stat(filepath.Join(tmpdir, "binary.gz"))
	if err != nil {
		return -1, nil, "", "", fmt.Errorf("stat binary.gz after writing: %v", err)
	}

	// Finally, add to the transparency log, creating the "recordnumber" file and
	// renaming tmpdir to the final directory in resultDir.
	recordNumber, err := addSum(ctx, tmpdir, br)
	if err != nil {
		return -1, nil, "", "", fmt.Errorf("adding sum to tranparency log: %w", err)
	}
	tmpdir = ""

	handleBinaryCacheAdd(st.Size())

	recentBuilds.Lock()
	recentBuilds.links = append(recentBuilds.links, request{bs, br.Sum, pageIndex}.link())
	if len(recentBuilds.links) > 10 {
		recentBuilds.links = recentBuilds.links[len(recentBuilds.links)-10:]
	}
	recentBuilds.Unlock()

	return recordNumber, &br, "", "", nil
}

func saveFailure(ctx context.Context, bs buildSpec, buildErr error, output, reason string) error {
	level := slog.LevelInfo
	if reason == "" {
		level = slog.LevelError
	}
	logger(ctx).Log(ctx, level, "build failure", "err", buildErr, "reason", reason, "buildspec", bs, "output", output)

	tmpdir, err := os.MkdirTemp(config.DataDir, "tmpfail")
	if err != nil {
		return err
	}
	defer func() {
		if tmpdir != "" {
			if err := os.RemoveAll(tmpdir); err != nil {
				logger(ctx).Error("removing result tmpdir for build failure", "err", err, "tmpdir", tmpdir)
			}
		}
	}()

	output = buildErr.Error() + "\n\n" + output
	if err := writeGz(filepath.Join(tmpdir, "log.gz"), strings.NewReader(output)); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmpdir, "builderror.txt"), fmt.Appendf(nil, "%s\n%v\n", bs, buildErr), 0666); err != nil {
		return err
	}

	fp := filepath.Join(config.DataDir, "buildfailures.txt")
	if f, err := os.OpenFile(fp, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0666); err != nil {
		logger(ctx).Error("open buildfailures.txt", "err", err)
	} else {
		_, err := fmt.Fprintln(f, bs.String())
		logCheck(ctx, err, "writing buildspec to buildfailures.txt")
		err = f.Close()
		logCheck(ctx, err, "close buildfailures.txt")
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

// For matching errors about compiling source files. Example:
//
//	go/pkg/mod/golang.org/x/sys@v0.0.0-20190507160741-ecd444e8653b/unix/syscall_unix_gc.go:12:6: missing function body
var reSourceError = regexp.MustCompile(`go/pkg/mod/.*\.go:[1-9][0-9]*:[1-9][0-9]*: `)

// cannotBuild takes output from a failed build and indicates the reason this
// build will never succeed, or an empty string otherwise.
func cannotBuild(output string) string {
	// "android/amd64 requires external (cgo) linking, but cgo is not enabled"
	if strings.Contains(output, "requires external (cgo) linking, but cgo is not enabled") {
		return "cgo"
	}

	// go: github.com/mjl-/sherpa/cmd/sherpaclient@v0.4.0: github.com/mjl-/sherpa@v0.4.0: parsing go.mod:
	//         module declares its path as: bitbucket.org/mjl/sherpa
	//                 but was required as: github.com/mjl-/sherpa
	if strings.Contains(output, "module declares its path as") && strings.Contains(output, "but was required as") {
		return "module path mismatch"
	}

	// go: github.com/mjl-/sherpa/cmd/sherpaclient@v0.4.1: version constraints conflict:
	if strings.Contains(output, ": version constraints conflict:") {
		return "version constraints conflict"
	}

	// go: unsupported GOOS/GOARCH pair openbsd/riscv64
	if strings.Contains(output, "go: unsupported GOOS/GOARCH pair ") {
		return "unsupported platform"
	}

	// loadinternal: cannot find runtime/cgo
	// .../gobuild/sdk/go1.21.5/pkg/tool/linux_amd64/link: running clang failed: exec: "clang": executable file not found in $PATH
	if strings.Contains(output, "loadinternal: cannot find runtime/cgo") {
		return "external linker"
	}

	// go/pkg/mod/github.com/mjl-/mox@v0.0.11/mlog/log.go:21:2: package log/slog is not in GOROOT (/home/service/gobuild/sdk/go1.20.2/src/log/slog)
	if strings.Contains(output, "is not in GOROOT (") {
		return "likely import of future standard library package"
	}

	// The go.mod file for the module providing named packages contains one or
	// more replace directives. It must not contain directives that would cause
	// it to be interpreted differently than if it were the main module.
	if strings.Contains(output, "The go.mod file for the module providing named packages contains one or") &&
		strings.Contains(output, "more replace directives. It must not contain directives that would cause") {
		return "go.mod has replace directives"
	}

	// note: module requires Go 1.19
	if strings.Contains(output, "note: module requires Go ") {
		return "requires newer go version"
	}

	// go/pkg/mod/github.com/mjl-/mox@v0.0.10/dns/mock.go:7:2: package slices is not in GOROOT .../sdk/go1.20.8/src/slices)
	// note: imported by a module that requires go 1.21"
	if strings.Contains(output, "note: imported by a module that requires go") {
		return "imported module requires newer go"
	}

	// package github.com/mjl-/mox
	// 	imports github.com/mjl-/bstore
	//	imports go.etcd.io/bbolt
	//	imports golang.org/x/sys/unix: build constraints exclude all Go files in .../go/pkg/mod/golang.org/x/sys@v0.7.0/unix
	if strings.Contains(output, ": build constraints exclude all Go files in") {
		return "build constraint excludes all go files"
	}

	// go/pkg/mod/github.com/mjl-/ding@v0.4.0/serve.go:52:14: undefined: syscall.Umask
	if strings.Contains(output, ": undefined: ") {
		return "undefined symbol likely only available in newer stdlib"
	}

	// go/pkg/mod/github.com/mjl-/ding@v0.4.0/serve.go:90:4: unknown field Credential in struct literal of type syscall.SysProcAttr
	if strings.Contains(output, ": unknown field ") {
		return "unknown field in struct likely only available in newer stdlib"
	}

	if reSourceError.MatchString(output) {
		return "error on go source line"
	}

	return ""
}
