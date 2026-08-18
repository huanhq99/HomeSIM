//go:build !windows

package main

import (
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestExternalVoicePasswordFileRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "asterisk-password-fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readExternalVoicePasswordFile(path, externalVoiceSecretMaxBytes)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO password path was accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO password path blocked before its type could be rejected")
	}
}
