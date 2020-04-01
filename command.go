package main

import (
	"os/exec"
)

func makeCommand(argv ...string) *exec.Cmd {
	var l []string
	l = append(l, config.Run...)
	l = append(l, argv...)
	return exec.Command(l[0], l[1:]...)
}
