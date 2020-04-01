package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"time"

	"golang.org/x/mod/module"
)

func ensureModule(gobin, mod, version string) (string, []byte, error) {
	modPath, err := module.EscapePath(mod)
	if err != nil {
		return "", nil, fmt.Errorf("bad module path: %v", err)
	}
	modVersion, err := module.EscapeVersion(version)
	if err != nil {
		return "", nil, fmt.Errorf("bad module version: %v", err)
	}
	modDir := homedir + "/go/pkg/mod/" + modPath + "@" + modVersion

	_, err = os.Stat(modDir)
	if err == nil {
		return modDir, nil, nil
	}

	if !os.IsNotExist(err) {
		return "", nil, fmt.Errorf("%w: checking if module is checked out locally: %v", errServer, err)
	}

	output, err := fetchModule(gobin, mod, version)
	if err != nil {
		return "", output, err
	}
	return modDir, nil, nil
}

func fetchModule(gobin, mod, version string) ([]byte, error) {
	dir, err := ioutil.TempDir("", "goget")
	if err != nil {
		return nil, fmt.Errorf("%w: tempdir for go get: %v", errServer, err)
	}
	defer os.RemoveAll(dir)

	t0 := time.Now()
	defer func() {
		metricGogetDuration.Observe(time.Since(t0).Seconds())
	}()
	cmd := makeCommand(gobin, "get", "-d", "-x", "-v", "--", mod+"@"+version)
	cmd.Dir = dir
	cmd.Env = []string{
		"GOPROXY=" + config.GoProxy,
		"GO111MODULE=on",
		"HOME=" + homedir,
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		metricGogetErrors.Inc()
		return output, fmt.Errorf("go get: %v", err)
	}
	return nil, nil
}
