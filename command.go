package main

import (
	"context"
	"log/slog"
	"os/exec"
	"path/filepath"
	"runtime"
)

// We allow at most 3 concurrent non-build commands, e.g. for fetching modules,
// listing main commands in a module, checking for cgo. When executing a command
// based on a web request, a command acquire token is required. Full builds are
// already managed by the coordinator and its command executions must not get a
// token.
var cmdacquirec = make(chan struct{}, 3)

func init() {
	// Fill with tokens.
	for range 3 {
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
func makeCommand(goversion string, withGoproxy bool, dir string, cgoEnabled bool, extraEnv []string, argv ...string) *exec.Cmd {
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
	cmd.Dir = dir
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
			"USERPROFILE="+homedir,
			"AppData="+filepath.Join(homedir, "AppData"),
			"LocalAppData="+filepath.Join(homedir, "LocalAppData"),
		)
	case "plan9":
		cmd.Env = append(cmd.Env, "home="+homedir)
	default:
		cmd.Env = append(cmd.Env, "HOME="+homedir)
	}
	if len(config.Environment) > 0 {
		cmd.Env = append(cmd.Env, config.Environment...)
	}
	if len(extraEnv) > 0 {
		cmd.Env = append(cmd.Env, extraEnv...)
	}
	slog.Debug("prepared command", "workdir", dir, "argv", l, "environment", cmd.Env)
	return cmd
}
