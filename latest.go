package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type modVersion struct {
	Version string
	Time    time.Time
}

func resolveModuleLatest(ctx context.Context, module string) (*modVersion, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	u := fmt.Sprintf("%s%s@latest", config.GoProxy, module)
	mreq, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: preparing goproxy http request: %v", errServer, err)
	}
	resp, err := http.DefaultClient.Do(mreq)
	if err != nil {
		return nil, fmt.Errorf("%w: http request to goproxy: %v", errServer, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%w: error response from goproxy, status %s", errRemote, resp.Status)
	}
	var info modVersion
	err = json.NewDecoder(resp.Body).Decode(&info)
	if err != nil {
		return nil, fmt.Errorf("%w: parsing json returned by goproxy: %v", errRemote, err)
	}
	if info.Version == "" {
		return nil, fmt.Errorf("%w: empty version from goproxy", errRemote)
	}
	return &info, nil
}
