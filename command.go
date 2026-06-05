package main

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
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
var cmdacquirec = make(chan struct{}, 5)

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

	// Copy gosumdb state if it exists.
	f, err := os.Open(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return dir, nil
		}
		return "", fmt.Errorf("open gosumdb state file %q: %v", statePath, err)
	}
	defer f.Close()
	if err := restoreState(ctx, f, dir); err != nil {
		return "", fmt.Errorf("restoring gosumdb state: %w", err)
	}
	return dir, nil
}

func removeCommandDir(ctx context.Context, cmdHomeDir string) {
	if err := saveState(ctx, cmdHomeDir); err != nil {
		logger(ctx).Error("saving gosumdb state, ignoring", "err", err)
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
	if err != nil {
		logger(ctx).Error("walk and change permissions before removal, continuing", "err", err, "dir", dir)
	}
}

// restoreState extracts the gosumdb state tar file to a destination directory.
func restoreState(ctx context.Context, f io.Reader, dstDir string) error {
	r := tar.NewReader(f)
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("next file in tar: %v", err)
		}
		fi := h.FileInfo()
		p := filepath.Join(dstDir, h.Name)
		if fi.IsDir() {
			if err := os.MkdirAll(p, 0o750); err != nil {
				return fmt.Errorf("mkdirall %q: %v", p, err)
			}
			continue
		} else if !fi.Mode().IsRegular() {
			return fmt.Errorf("not a regular file in tar file: %q: %o", h.Name, fi.Mode())
		}
		os.MkdirAll(filepath.Dir(p), 0o750)
		nf, err := os.Create(p)
		if err != nil {
			return fmt.Errorf("create %q: %v", p, err)
		}
		if _, err := io.Copy(nf, r); err != nil {
			if xerr := nf.Close(); xerr != nil {
				logger(ctx).Error("closing new file after error", "err", xerr, "path", p)
			}
		}
		if err := nf.Close(); err != nil {
			return fmt.Errorf("closing new file %q: %v", p, err)
		}
	}
	return nil
}

// saveState stores the gosumdb state from a "build home dir" to the state tar
// file. The current file is untouched if an error occurrs.
func saveState(ctx context.Context, srcDir string) error {
	f, err := os.CreateTemp(filepath.Dir(statePath), "gosumdbstate-tmp.")
	if err != nil {
		return fmt.Errorf("create temp gosumdb state file: %w", err)
	}

	defer func() {
		if f == nil {
			return
		}
		name := f.Name()
		err := f.Close()
		logCheck(ctx, err, "closing temp gosumdb state file after error")
		err = os.Remove(name)
		logCheck(ctx, err, "removing temp gosumdb state file after error")
	}()

	w := tar.NewWriter(f)

	addTree := func(dir string) error {
		return filepath.WalkDir(filepath.Join(srcDir, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return fmt.Errorf("walk: %v", err)
			}
			fi, err := d.Info()
			if err != nil {
				return fmt.Errorf("fileinfo for entry: %v", err)
			}
			// We only add regular files.
			if d.IsDir() {
				return nil
			} else if !fi.Mode().IsRegular() {
				return fmt.Errorf("not a regular file %q: mode %o", path, fi.Mode())
			}
			h, err := tar.FileInfoHeader(fi, "")
			if err != nil {
				return fmt.Errorf("fileinfo to tar header for %q: %v", path, err)
			}
			h.Name, err = filepath.Rel(srcDir, path)
			if err != nil {
				return fmt.Errorf("relative path for %q in %q: %v", path, srcDir, err)
			}
			if err := w.WriteHeader(h); err != nil {
				return fmt.Errorf("write header: %v", err)
			}
			xf, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("open %q: %v", path, err)
			}
			defer xf.Close()
			if n, err := io.Copy(w, xf); err != nil {
				return fmt.Errorf("copy %q to gosumdb state file: %w", path, err)
			} else if int64(n) != h.Size {
				return fmt.Errorf("copied %d bytes for path %q to gosumdb state file, expected %d", n, path, h.Size)
			}
			return nil
		})
	}

	// Copy back gosumdb state.
	paths := []string{
		"go/pkg/sumdb/sum.golang.org",
		"go/pkg/mod/cache/download/sumdb/sum.golang.org/tile",
	}
	for _, p := range paths {
		if err := addTree(p); err != nil {
			return fmt.Errorf("copying gosumdb state to tmp tar file: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("closing temp gosumdb state tar writer: %v", err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp gosumdb state file: %w", err)
	}
	if err := os.Rename(name, statePath); err != nil {
		return fmt.Errorf("renaming temp gosumdb state file %q to final name %q: %v", name, statePath, err)
	}
	f = nil
	return nil
}
