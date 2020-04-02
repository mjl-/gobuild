package main

import (
	"os/exec"
	"path/filepath"
)

func makeCommand(argv ...string) *exec.Cmd {
	var l []string
	l = append(l, config.Run...)
	l = append(l, argv...)
	cmd := exec.Command(l[0], l[1:]...)
	cmd.Env = []string{
		"GOPROXY=" + config.GoProxy,
		"GO111MODULE=on",
		"CGO_ENABLED=0",
		"GO19CONCURRENTCOMPILATION=0",
		"HOME=" + homedir,
		"USERPROFILE=" + homedir,
		"AppData=" + filepath.Join(homedir, "AppData"),
		"LocalAppData=" + filepath.Join(homedir, "LocalAppData"),
	}
	return cmd
}
