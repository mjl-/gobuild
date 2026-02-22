package main

import (
	"context"
	"net/http"
)

const userAgent = "Go-http-client/1.1 (https://github.com/mjl-/gobuild)"

func httpGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	return http.DefaultClient.Do(req)
}
