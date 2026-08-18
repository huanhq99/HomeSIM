package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/iniwex5/vohive/internal/publicedgeserver"
)

func main() {
	config, err := loadEdgeRuntimeConfig(os.LookupEnv, newEdgeAccessHTTPClient())
	if err != nil {
		log.Fatal("public edge startup refused: error_code=edge_configuration_invalid")
	}
	edge, err := publicedgeserver.New(publicedgeserver.Config{
		Registry: config.registry, ExactHost: config.exactHost,
		HTTPAuth: edgeAccessAuth{verifier: config.access},
	})
	if err != nil {
		log.Fatal("public edge startup refused: error_code=edge_configuration_invalid")
	}
	server := &http.Server{
		Addr: config.listenAddress, Handler: edge,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	listener, err := net.Listen("tcp", config.listenAddress)
	if err != nil {
		log.Fatal("public edge startup refused: error_code=edge_listener_unavailable")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	log.Printf("Public edge read-only service is ready")
	select {
	case err := <-serveResult:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Printf("public edge stopped unexpectedly: error_code=edge_http_unavailable")
		}
	case <-ctx.Done():
	}
	_ = edge.Close()
	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		log.Printf("public edge shutdown incomplete: error_code=edge_shutdown_failed")
	}
}

func newEdgeAccessHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS13},
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 5 * time.Second,
			ExpectContinueTimeout: time.Second,
			MaxIdleConns:          4,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       30 * time.Second,
		},
	}
}
