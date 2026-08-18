//go:build windows

package main

import (
	"errors"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func initPlatformRuntime() func() {
	base := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
	if base == "" {
		base = os.TempDir()
	}
	logDir := filepath.Join(base, "DJOneHub", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return func() {}
	}
	file, err := os.OpenFile(filepath.Join(logDir, "djonehub.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return func() {}
	}
	log.SetOutput(io.MultiWriter(file))
	log.Printf("DJOneHub Windows is starting")
	return func() { _ = file.Close() }
}

func openPlatformUI(url string) {
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			conn, err := net.DialTimeout("tcp", "127.0.0.1:7576", 250*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				break
			}
			time.Sleep(100 * time.Millisecond)
		}

		if startEdgeApp(url) {
			return
		}
		if err := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", url).Start(); err != nil {
			log.Printf("open desktop UI: %v", err)
		}
	}()
}

func platformOpenExistingUI(url string) bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:7576", 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	openPlatformUI(url)
	return true
}

func startEdgeApp(url string) bool {
	var candidates []string
	for _, base := range []string{os.Getenv("PROGRAMFILES(X86)"), os.Getenv("PROGRAMFILES"), os.Getenv("LOCALAPPDATA")} {
		if strings.TrimSpace(base) == "" {
			continue
		}
		candidates = append(candidates,
			filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe"),
		)
	}
	for _, path := range candidates {
		if _, err := os.Stat(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			continue
		}
		if err := exec.Command(path, "--app="+url, "--start-windowed").Start(); err == nil {
			return true
		}
	}
	return false
}
