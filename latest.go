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
		return nil, fmt.Errorf("preparing goproxy http request: %v", err)
	}
	resp, err := http.DefaultClient.Do(mreq)
	if err != nil {
		return nil, fmt.Errorf("http request to goproxy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("error response from goproxy, status %s", resp.Status)
	}
	var info modVersion
	err = json.NewDecoder(resp.Body).Decode(&info)
	if err != nil {
		return nil, fmt.Errorf("parsing json returned by goproxy: %v", err)
	}
	if info.Version == "" {
		return nil, fmt.Errorf("empty version from goproxy")
	}
	return &info, nil
}
