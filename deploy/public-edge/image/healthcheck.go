package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

const (
	healthURL  = "http://127.0.0.1:8080/healthz"
	healthHost = "phone.example.com"
)

func main() {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			DisableKeepAlives:     true,
			MaxIdleConns:          0,
			ResponseHeaderTimeout: 2 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	request, err := http.NewRequest(http.MethodGet, healthURL, nil)
	if err != nil {
		os.Exit(1)
	}
	request.Host = healthHost
	response, err := client.Do(request)
	if err != nil {
		os.Exit(1)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64))
	if err != nil || response.StatusCode != http.StatusOK ||
		response.Header.Get("Content-Type") != "application/json" ||
		response.Header.Get("Cache-Control") != "no-store" ||
		!bytes.Equal(body, []byte(`{"status":"ok"}`)) {
		os.Exit(1)
	}
}
