package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/mod/module"
)

var (
	errNotExist   = errors.New("does not exist")
	errBadModule  = errors.New("bad module")
	errBadVersion = errors.New("bad version")
)

func ensureModule(gobin, mod, version string) (string, []byte, error) {
	modPath, err := module.EscapePath(mod)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", errBadModule, err)
	}
	modVersion, err := module.EscapeVersion(version)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", errBadVersion, err)
	}
	modDir := filepath.Join(homedir, "go", "pkg", "mod", filepath.Clean(modPath)+"@"+modVersion)

	_, err = os.Stat(modDir)
	if err == nil {
		return modDir, nil, nil
	}

	if !os.IsNotExist(err) {
		return "", nil, fmt.Errorf("%w: checking if module is checked out locally: %v", errServer, err)
	}

	// todo: for errors, want to know if module or version does not exist. probably requires parsing the error message for: 1. no module; 2. no version; 3. no package.
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
	cgo := true
	cmd := makeCommand(cgo, gobin, "get", "-d", "-x", "-v", "--", mod+"@"+version)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		metricGogetErrors.Inc()
		return output, fmt.Errorf("go get: %v", err)
	}
	return nil, nil
}
