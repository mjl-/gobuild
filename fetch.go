package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
)

func ensureModule(gobin, module, version string) (string, []byte, error) {
	modDir := homedir + "/go/pkg/mod/" + module[:len(module)-1] + "@" + version

	_, err := os.Stat(modDir)
	if err == nil {
		return modDir, nil, nil
	}

	if !os.IsNotExist(err) {
		return "", nil, fmt.Errorf("checking if module is checked out locally: %v", err)
	}

	output, err := fetchModule(gobin, module, version)
	if err != nil {
		return "", output, err
	}
	return modDir, nil, nil
}

func fetchModule(gobin, module, version string) ([]byte, error) {
	dir, err := ioutil.TempDir("", "goget")
	if err != nil {
		return nil, fmt.Errorf("tempdir for go get: %v", err)
	}
	defer os.RemoveAll(dir)

	cmd := exec.Command(gobin, "get", "-d", "-v", module[:len(module)-1]+"@"+version)
	cmd.Dir = dir
	cmd.Env = []string{
		fmt.Sprintf("GOPROXY=%s", config.GoProxy),
		"GO111MODULE=on",
		"HOME=" + homedir,
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("go get: %v", err)
	}
	return nil, nil
}
