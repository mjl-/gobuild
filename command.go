package main

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// We allow one concurrent non-build commands, e.g. for fetching modules,
// listing main commands in a module, checking for cgo. When executing a command
// based on a web request, a command acquire token is required. Full builds are
// already managed by the coordinator and its command executions must not get a
// token.
// Once we isolate builds more properly, we can increase concurrency again.
var cmdacquirec = make(chan struct{}, 1)

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
		return func() {
			cmdacquirec <- struct{}{}
		}, nil
	}
}

// Prepare command, typically for running go get. We sometimes need CGO_ENABLED to
// properly list the cgo files that would be used during a build. Only set
// withGoproxy for downloading modules, not doing builds or listing packages.
func makeCommand(cmdHomeDir, workDir, goversion string, withGoproxy bool, cgoEnabled bool, extraEnv []string, argv ...string) *exec.Cmd {
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
	cmd := exec.Command(l[0], l[1:]...)
	cmd.Dir = workDir
	cmd.Env = []string{
		goproxy,
		cgo,
		"GO111MODULE=on",
		"GO19CONCURRENTCOMPILATION=0",
		"GOTOOLCHAIN=" + goversion,
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
	slog.Debug("prepared command", "cmdhomedir", cmdHomeDir, "workdir", workDir, "argv", l, "environment", cmd.Env)
	return cmd
}

func newCommandDir(name string) (string, error) {
	dir, err := os.MkdirTemp(commandDir, name+"-")
	if err != nil {
		slog.Error("creating temp dir for executing command", "err", err, "name", name)
		return "", err
	}

	// Copy gosumdb state if it exists.
	stateDir := filepath.Join(homedir, "gosumdbstate")
	if _, err := os.Stat(stateDir); err == nil {
		if err := copyFiletree(dir, stateDir); err != nil {
			return "", fmt.Errorf("copying gosumdb state: %w", err)
		}
	}

	return dir, err
}

func removeCommandDir(cmdHomeDir string) {
	// Copy back gosumdb state.
	var ok = true
	paths := []string{
		"go/pkg/sumdb/sum.golang.org",
		"go/pkg/mod/cache/download/sumdb/sum.golang.org/tile",
	}
	for _, p := range paths {
		if err := copyFiletree(filepath.Join(cmdHomeDir, "gosumdbstate", p), filepath.Join(cmdHomeDir, p)); err != nil {
			slog.Error("copying gosumdb state to tmp dir, continuing without new gosumdb state", "err", err, "path", p)
			ok = false
		}
	}
	if ok {
		stateDir := filepath.Join(homedir, "gosumdbstate")
		random := make([]byte, 12)
		cryptorand.Read(random)
		oldStateDir := filepath.Join(homedir, "gosumdbstate-"+base64.RawURLEncoding.EncodeToString(random))
		if err := os.Rename(stateDir, oldStateDir); err != nil {
			slog.Error("moving old gosumdb state dir out of the way", "err", err, "srcdir", stateDir, "dstdir", oldStateDir)
		} else if err := os.Rename(filepath.Join(cmdHomeDir, "gosumdbstate"), stateDir); err != nil {
			slog.Error("moving new gosumdb state dir in place", "err", err)
		} else if err := os.RemoveAll(oldStateDir); err != nil {
			slog.Error("removing old gosumdb state dir", "err", err, "oldstatedir", oldStateDir)
		}
	}

	// Walk cmdHomeDir to change permissions of go/pkg/mod directory so we can remove it all.
	pkgmodDir := filepath.Join(cmdHomeDir, "go/pkg/mod")
	err := filepath.WalkDir(pkgmodDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(path, 0o750)
		}
		return os.Chmod(path, 0o640)
	})
	if err != nil {
		slog.Error("walk and change permissions before removal, continuing", "err", err, "dir", pkgmodDir)
	}
	if err := os.RemoveAll(cmdHomeDir); err != nil {
		slog.Error("removing command dir", "err", err, "cmdhomedir", cmdHomeDir)
	}
}

func copyFiletree(dstDir, srcDir string) error {
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Determine destination path.
		if !strings.HasPrefix(path, srcDir) {
			return fmt.Errorf("walked path %q is not prefixed by requested dir %q", path, srcDir)
		}
		dst := strings.TrimPrefix(path[len(srcDir):], "/")
		if dst == "" {
			// We do nothing for the state root directory.
			return nil
		}
		dst = filepath.Join(dstDir, dst)

		// We don't explicitly create directories.
		if d.IsDir() {
			return nil
		}

		// Ensure dir exists. We ignore errors, the create below will fail if the dir doesn't exist.
		os.MkdirAll(filepath.Dir(dst), 0o750)

		// We copy files.
		sf, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %q: %v", path, err)
		}
		defer sf.Close()
		df, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o640)
		if err != nil {
			return fmt.Errorf("create %q: %v", dst, err)
		}
		defer func() {
			if df != nil {
				df.Close()
			}
		}()
		if _, err := io.Copy(df, sf); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
		err = df.Close()
		df = nil
		return err
	})
}
