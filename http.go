package main

import (
	"net/http"
)

const userAgent = "Go-http-client/1.1 (https://github.com/mjl-/gobuild)"

func httpGet(url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	return http.DefaultClient.Do(req)
}
