package main

import (
	"context"
	"errors"
	"fmt"
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

// Fetches module@version for use in subsequent build.
func ensureModule(ctx context.Context, goversion, gobin, mod, version string) (string, []byte, error) {
	modPath, err := module.EscapePath(mod)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", errBadModule, err)
	}
	modVersion, err := module.EscapeVersion(version)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", errBadVersion, err)
	}
	modDir := filepath.Join(homedir, "go", "pkg", "mod", filepath.Clean(modPath)+"@"+modVersion)

	if _, err := os.Stat(modDir); err == nil {
		return modDir, nil, nil
	} else if !os.IsNotExist(err) {
		return "", nil, fmt.Errorf("%w: checking if module is checked out locally: %v", errServer, err)
	}

	// todo: for errors, want to know if module or version does not exist. probably requires parsing the error message for: 1. no module; 2. no version; 3. no package.
	if output, err := fetchModule(ctx, goversion, modDir, gobin, mod, version); err != nil {
		return "", output, err
	}
	return modDir, nil, nil
}

func fetchModule(ctx context.Context, goversion, modDir, gobin, mod, version string) ([]byte, error) {
	t0 := time.Now()
	defer func() {
		metricGogetDuration.Observe(time.Since(t0).Seconds())
	}()
	goproxy := true
	cgo := true
	gv, err := parseGoVersion(goversion)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", errBadGoversion, err)
	}
	if release, err := commandAcquire(ctx); err != nil {
		return nil, err
	} else {
		defer release()
	}

	if gv.major == 1 && gv.minor >= 18 {
		// Go1.18 dropped "go get -d" for downloading modules. Using "go mod download
		// <module>@<version>" downloads the module, we get the dependencies by running "go
		// mod download" again in the checked out module path.
		cmd := makeCommand(goversion, goproxy, emptyDir, cgo, nil, gobin, "mod", "download", "-x", "--", mod+"@"+version)
		output, err := cmd.CombinedOutput()
		if err != nil {
			metricGogetErrors.Inc()
			return output, fmt.Errorf("go mod download module: %v", err)
		}

		cmd = makeCommand(goversion, goproxy, modDir, cgo, nil, gobin, "mod", "download", "-x")
		if output2, err := cmd.CombinedOutput(); err != nil {
			metricGogetErrors.Inc()
			return append(output, output2...), fmt.Errorf("go mod download dependencies: %v", err)
		}
	} else {
		cmd := makeCommand(goversion, goproxy, emptyDir, cgo, nil, gobin, "get", "-d", "-x", "-v", "--", mod+"@"+version)
		if output, err := cmd.CombinedOutput(); err != nil {
			metricGogetErrors.Inc()
			return output, fmt.Errorf("go get: %v", err)
		}
	}
	return nil, nil
}
