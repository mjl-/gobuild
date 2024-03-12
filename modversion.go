package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type modVersion struct {
	Version string
	Time    time.Time
}

func resolveModuleVersion(ctx context.Context, mod, version string) (*modVersion, error) {
	t0 := time.Now()
	defer func() {
		metricGoproxyResolveVersionDuration.Observe(time.Since(t0).Seconds())
	}()

	goversion, _, _ := installedSDK()
	if goversion == "" {
		return nil, fmt.Errorf("no supported go toolchains available: %w", errServer)
	}
	gobin, err := ensureGobin(goversion)
	if err != nil {
		return nil, fmt.Errorf("ensuring go version is available: %v (%w)", err, errTempFailure)
	}

	const goproxy = true
	const cgo = false
	cmd := makeCommand(goversion, goproxy, emptyDir, cgo, nil, gobin, "list", "-x", "-m", "-json", "--", mod+"@"+version)
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("resolving module version: %v (error output: %q)", err, stderr.String())
	}

	var info modVersion
	if err := json.Unmarshal(output, &info); err != nil {
		return nil, fmt.Errorf("%w: parsing json returned by goproxy: %v", errRemote, err)
	} else if info.Version == "" {
		return nil, fmt.Errorf("%w: empty version from goproxy", errRemote)
	}
	return &info, nil
}
