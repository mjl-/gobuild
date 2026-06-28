package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/mjl-/bstore"
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
	result *Record
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
// If exp is non-nil, a build was done in the past but the binary removed.
// This build will restore the binary. If exp is empty and the build is
// successful, a record is added to the transparency log.
//
// On success, returns a build result.
// On failure, returns an error (last value), and optionally the output as
// string followed by a reason in case this build won't succeed in the future
// (e.g. due to invalid code and/or compiler combination).
func build(ctx context.Context, bs buildSpec, exp *BuildResult) (*BuildResult, string, string, error) {
	targets.increase(bs.Goos + "/" + bs.Goarch)

	gobin, err := ensureGobin(bs.Goversion)
	if err != nil {
		return nil, "", "", fmt.Errorf("ensuring go version is available: %v (%w)", err, errTempFailure)
	}

	cmdDir, err := newCommandDir(ctx, "build")
	if err != nil {
		return nil, "", "", fmt.Errorf("creating temporary command directory: %v (%w)", err, errTempFailure)
	}
	defer removeCommandDir(ctx, cmdDir)

	if _, output, err := ensureModule(ctx, cmdDir, bs.Goversion, gobin, bs.Mod, bs.Version); err != nil {
		return nil, "", "", fmt.Errorf("error fetching module from goproxy for build: %v (%w)\n\n# output from go get:\n%s", err, errTempFailure, output)
	}

	// Launch goroutines to let the verifiers build the same code and return their
	// build result. After our build, we verify we all had the same result. If our
	// build fails, we just ignore these results, and let the remote builds continue.
	// They will not cancel the build anyway.

	verifyResult := make(chan remoteBuild, len(config.Verifiers)+len(config.VerifierURLs))
	verifyLink := request{bs, nil, pageRecord}.link()

	// Verify a build at remote based on Verifier, which looks up a build in the
	// remote's transparency log.
	verify := func(v Verifier) (*Record, error) {
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
	verifyURL := func(verifierBaseURL string) (*Record, error) {
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
		if resp.StatusCode != http.StatusOK {
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
			return nil, "", "", fmt.Errorf("making temp dir: %v", err)
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
		return nil, "", "", fmt.Errorf("attempting to remove preexisting binary: %v (%w)", err, errTempFailure)
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
		return nil, "", "", fmt.Errorf("%w: %s", errBadGoversion, err)
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
			return nil, "", reason, fmt.Errorf("storing results of failure: %v (%w)", xerr, errTempFailure)
		}
		return nil, out, reason, err
	}

	// Where we temporarily store the gzipped binary.
	tmpdir, err := os.MkdirTemp(config.DataDir, "tmp-binaries-")
	if err != nil {
		return nil, "", "", err
	}
	defer func() {
		if err := os.RemoveAll(tmpdir); err != nil {
			logger(ctx).Error("remove binary tmp dir", "err", err, "tmpdir", tmpdir)
		}
	}()

	br := Record{
		buildSpec: bs,
	}

	// Calculate our hash.
	rf, err := os.Open(resultPath)
	if err != nil {
		return nil, "", "", fmt.Errorf("open result: %v", err)
	}
	defer rf.Close()

	if info, err := rf.Stat(); err != nil {
		return nil, "", "", fmt.Errorf("stat result: %v", err)
	} else {
		br.Filesize = info.Size()
	}

	h := sha256.New()
	if _, err := io.Copy(h, rf); err != nil {
		return nil, "", "", fmt.Errorf("read result: %v", err)
	} else if _, err := rf.Seek(0, 0); err != nil {
		return nil, "", "", fmt.Errorf("seek result: %v", err)
	}
	br.Sum = buildSum{[20]byte(h.Sum(nil)[:20])}

	// Called after a binary is added to the data binaries directory. Either when
	// recreating the binary, or when first creating it.
	handleBinaryCacheAdd := func(size int64) {
		// Cache size administration isn't entirely race-free. Multiple processes may
		// finish a build and a positive number of bytes to reclaim. Perhaps a little too
		// many or too few files will get cleaned up.
		reclaim := binaryCacheSizeAdd(size)
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
	if exp != nil {
		if br.Sum != exp.TreeRecord.Sum {
			metricRecompileMismatch.WithLabelValues(bs.Goos, bs.Goarch, bs.Goversion).Inc()
			return nil, "", "", fmt.Errorf("sum of rebuilt binary %s does not match previous sum %s", br.Sum, exp.TreeRecord.Sum)
		}
		p := filepath.Join(config.DataDir, "binaries", fmt.Sprintf("%d.gz", exp.TreeRecord.ID))
		ptmp := filepath.Join(tmpdir, "binary.gz")
		if size, err := writeGz(ptmp, rf); err != nil {
			return nil, "", "", err
		} else if err := os.Rename(ptmp, p); err != nil {
			return nil, "", "", fmt.Errorf("moving binary.gz to destination: %w", err)
		} else {
			handleBinaryCacheAdd(size)
			if exp.Result.FileSizeGz == 0 {
				exp.Result.FileSizeGz = size
				if err := database.Update(ctx, &exp.Result); err != nil {
					return nil, "", "", fmt.Errorf("updating gzipped filesize in database: %w", err)
				}
			}

			return exp, "", "", nil
		}
	}

	// Verify the sums of the verifiers.
	matchesFrom := []string{}
	mismatches := []string{}
	for n := len(config.Verifiers) + len(config.VerifierURLs); n > 0; n-- {
		vr := <-verifyResult
		if vr.err != nil {
			return nil, "", "", fmt.Errorf("build at verifier failed: %v (%w)", vr.err, errTempFailure)
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
			return nil, "", "", fmt.Errorf("build mismatches, we and %d others got %s, but %s (%w)", len(matchesFrom), br.Sum, strings.Join(mismatches, ", "), errTempFailure)
		}
		return nil, "", "", fmt.Errorf("build mismatches, we got %s, but %s (%w)", br.Sum, strings.Join(mismatches, ", "), errTempFailure)
	}

	// Write binary.
	binaryPath := filepath.Join(tmpdir, "binary.gz")
	var filesizeGz int64
	if size, err := writeGz(binaryPath, rf); err != nil {
		return nil, "", "", err
	} else {
		filesizeGz = size
	}

	// Compress build log.
	buildLogGz, err := compress([]byte(output))
	if err != nil {
		return nil, "", "", fmt.Errorf("compressing build log: %v", err)
	}

	// Finally, add to the transparency log.
	nbr, err := addSum(ctx, br, buildLogGz, binaryPath, filesizeGz)
	if err != nil {
		return nil, "", "", fmt.Errorf("adding sum to tranparency log: %w", err)
	}

	handleBinaryCacheAdd(filesizeGz)

	recentBuilds.Lock()
	recentBuilds.links = append(recentBuilds.links, request{bs, &br.Sum, pageIndex}.link())
	if len(recentBuilds.links) > 10 {
		recentBuilds.links = recentBuilds.links[len(recentBuilds.links)-10:]
	}
	recentBuilds.Unlock()

	return &nbr, "", "", nil
}

func saveFailure(ctx context.Context, bs buildSpec, buildErr error, output, reason string) error {
	level := slog.LevelInfo
	switch reason {
	case "", "unknown":
		level = slog.LevelError
	}
	logger(ctx).Log(ctx, level, "build failure", "err", buildErr, "reason", reason, "buildspec", bs)

	outputGz, err := compress([]byte(buildErr.Error() + "\n\n" + output))
	if err != nil {
		return fmt.Errorf("compressing build log: %v", err)
	}

	result := bs.result()
	result.ErrorReason = reason
	return database.Write(ctx, func(tx *bstore.Tx) error {
		if err := tx.Insert(&result); err != nil {
			return err
		}

		bl := BuildLog{ID: result.ID, Data: outputGz}
		if err := tx.Insert(&bl); err != nil {
			return err
		}

		return nil
	})
}

func compress(buf []byte) ([]byte, error) {
	var b bytes.Buffer
	gzw := gzip.NewWriter(&b)
	if _, err := gzw.Write(buf); err != nil {
		gzw.Close()
		return nil, err
	}
	err := gzw.Close()
	return b.Bytes(), err
}

func writeGz(path string, src io.Reader) (dstSize int64, rerr error) {
	lf, err := os.Create(path)
	if err != nil {
		return -1, err
	}
	defer func() {
		if lf != nil {
			lf.Close()
		}
	}()
	sz := &sizeWriter{w: lf}
	lfgz := gzip.NewWriter(sz)
	if _, err := io.Copy(lfgz, src); err != nil {
		return -1, err
	} else if err := lfgz.Close(); err != nil {
		return -1, err
	} else {
		err = lf.Close()
		lf = nil
		return sz.size, err
	}
}

type sizeWriter struct {
	w    io.Writer
	size int64
}

func (w *sizeWriter) Write(buf []byte) (int, error) {
	n, err := w.w.Write(buf)
	if n > 0 {
		w.size += int64(n)
	}
	return n, err
}

// For matching errors about compiling source files. Example:
//
//	go/pkg/mod/golang.org/x/sys@v0.0.0-20190507160741-ecd444e8653b/unix/syscall_unix_gc.go:12:6: missing function body
var reSourceError = regexp.MustCompile(`go/pkg/mod/.*\.go:[1-9][0-9]*:[1-9][0-9]*: `)

// cannotBuild takes output from a failed build and indicates the reason this
// build will never succeed, or string "unknown" otherwise.
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

	return "unknown"
}
