package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// We allow multiple concurrent non-build commands, e.g. for fetching modules,
// listing main commands in a module, checking for cgo. When executing a command
// based on a web request, a command acquire token is required. Full builds are
// already managed by the coordinator and its command executions must not get a
// token.
// Once we isolate builds more properly, we can increase concurrency again.
var cmdacquirec = make(chan struct{}, 15)

func init() {
	// Fill with tokens.
	for range cap(cmdacquirec) {
		cmdacquirec <- struct{}{}
	}
}

func commandAcquire(ctx context.Context) (release func(), err error) {
	select {
	case <-ctx.Done():
		return func() {}, ctx.Err()

	case <-cmdacquirec:
		metricCommandsBusy.Inc()
		return func() {
			metricCommandsBusy.Dec()
			cmdacquirec <- struct{}{}
		}, nil
	}
}

// Prepare command, typically for running go get. We sometimes need CGO_ENABLED to
// properly list the cgo files that would be used during a build. Only set
// withGoproxy for downloading modules, not doing builds or listing packages.
func makeCommand(ctx context.Context, cmdHomeDir, workDir, goversion string, withGoproxy bool, cgoEnabled bool, extraEnv []string, argv ...string) *exec.Cmd {
	cgo := "CGO_ENABLED=0"
	if cgoEnabled {
		cgo = "CGO_ENABLED=1"
	}

	goproxy := "GOPROXY=off"
	if withGoproxy {
		goproxy = "GOPROXY=" + config.GoProxy
	}

	var l []string
	l = append(l, config.Run...)
	l = append(l, argv...)
	cmd := exec.CommandContext(ctx, l[0], l[1:]...)
	cmd.Dir = workDir
	cmd.Env = []string{
		goproxy,
		cgo,
		"GO111MODULE=on",
		"GOTOOLCHAIN=" + goversion,
	}
	if gv, err := parseGoVersion(goversion); err != nil {
		logger(ctx).Error("parsing go version", "err", err, "version", goversion)
	} else if gv.major == 1 && gv.minor < 26 {
		// go1.26 removed GO19CONCURRENTCOMPILATION
		cmd.Env = append(cmd.Env, "GO19CONCURRENTCOMPILATION=0")
	}

	switch runtime.GOOS {
	case "windows":
		cmd.Env = append(cmd.Env,
			"USERPROFILE="+cmdHomeDir,
			"AppData="+filepath.Join(cmdHomeDir, "AppData"),
			"LocalAppData="+filepath.Join(cmdHomeDir, "LocalAppData"),
		)
	case "plan9":
		cmd.Env = append(cmd.Env, "home="+cmdHomeDir)
	default:
		cmd.Env = append(cmd.Env, "HOME="+cmdHomeDir)
	}
	if len(config.Environment) > 0 {
		cmd.Env = append(cmd.Env, config.Environment...)
	}
	if len(extraEnv) > 0 {
		cmd.Env = append(cmd.Env, extraEnv...)
	}
	logger(ctx).Debug("prepared command", "cmdhomedir", cmdHomeDir, "workdir", workDir, "argv", l, "environment", cmd.Env)
	return cmd
}

func newCommandDir(ctx context.Context, name string) (rdir string, rerr error) {
	dir, err := os.MkdirTemp(commandDir, name+"-")
	if err != nil {
		logger(ctx).Error("creating temp dir for executing command", "err", err, "name", name)
		return "", err
	}

	defer func() {
		if rerr != nil {
			if err := os.RemoveAll(dir); err != nil {
				logger(ctx).Error("cleaning up temp dir after failure", "err", err, "dir", dir)
			}
		}
	}()

	// Copy sumdb latest if it exists.
	buf, err := os.ReadFile(latestPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return dir, nil
		}
		return "", fmt.Errorf("open sumdb latest file %q: %v", latestPath, err)
	}
	p := filepath.Join(dir, "go/pkg/sumdb/sum.golang.org/latest")
	os.MkdirAll(filepath.Dir(p), 0o700)
	logCheck(ctx, err, "creating path to sumdb latest")
	if err := os.WriteFile(p, buf, 0o600); err != nil {
		return "", fmt.Errorf("restoring sumdb latest: %w", err)
	}

	return dir, nil
}

func removeCommandDir(ctx context.Context, cmdHomeDir string) {
	if err := saveLatest(ctx, cmdHomeDir); err != nil {
		logger(ctx).Error("saving sumdb latest, ignoring", "err", err)
	}

	// Walk cmdHomeDir to change permissions of go/pkg/mod directory so we can remove it all.
	pkgmodDir := filepath.Join(cmdHomeDir, "go/pkg/mod")
	chmodRecursive(ctx, pkgmodDir)

	if err := os.RemoveAll(cmdHomeDir); err != nil {
		logger(ctx).Error("removing command dir", "err", err, "cmdhomedir", cmdHomeDir)
	}
}

func chmodRecursive(ctx context.Context, dir string) {
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(path, 0o750)
		}
		return os.Chmod(path, 0o640)
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		logger(ctx).Error("walk and change permissions before removal, continuing", "err", err, "dir", dir)
	}
}

// saveLatest stores the sumdb latest from a "build home dir" to latestPath. The
// current file is untouched if an error occurrs.
func saveLatest(ctx context.Context, srcDir string) error {
	p := filepath.Join(srcDir, "go/pkg/sumdb/sum.golang.org/latest")
	buf, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open go/pkg/sumdb/sum.golang.org/latest: %v", err)
	}

	nf, err := os.CreateTemp(filepath.Dir(latestPath), "sumdb-latest-tmp.")
	if err != nil {
		return fmt.Errorf("create temp sumdb latest file: %w", err)
	}
	tmpname := nf.Name()
	defer func() {
		if tmpname != "" {
			err := os.Remove(tmpname)
			logCheck(ctx, err, "removing temp sumdb latest file after error")
		}
	}()
	err = nf.Close()
	logCheck(ctx, err, "closing temp sumdb latest file")
	if err := os.WriteFile(tmpname, buf, 0o600); err != nil {
		return fmt.Errorf("writing temp sumdb latest file: %w", err)
	}

	if err := os.Rename(tmpname, latestPath); err != nil {
		return fmt.Errorf("renaming temp sumdb latest file %q to final name %q: %v", tmpname, latestPath, err)
	}
	tmpname = ""
	return nil
}
