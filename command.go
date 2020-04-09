package main

import (
	"os/exec"
	"path/filepath"
)

func makeCommand(cgoEnabled bool, argv ...string) *exec.Cmd {
	cgo := "CGO_ENABLED=0"
	if cgoEnabled {
		cgo = "CGO_ENABLED=1"
	}

	var l []string
	l = append(l, config.Run...)
	l = append(l, argv...)
	cmd := exec.Command(l[0], l[1:]...)
	cmd.Env = append([]string{
		"GOPROXY=" + config.GoProxy,
		"GO111MODULE=on",
		cgo,
		"GO19CONCURRENTCOMPILATION=0",
		"HOME=" + homedir,
		"USERPROFILE=" + homedir,
		"AppData=" + filepath.Join(homedir, "AppData"),
		"LocalAppData=" + filepath.Join(homedir, "LocalAppData"),
	}, config.Environment...)
	return cmd
}
