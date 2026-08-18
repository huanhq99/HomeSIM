//go:build !darwin

package main

import (
	"errors"
	"net"
	"os"
)

func remoteMediaControlPlatformSupported() error {
	return errors.New("remote media control socket requires macOS")
}

func secureRemoteMediaControlSocket(string) error {
	return errors.New("remote media control socket requires macOS")
}

func remoteMediaControlPeerEUID(*net.UnixConn) (uint32, error) {
	return 0, errors.New("remote media peer credentials require macOS")
}

func fileOwnedByCurrentUser(os.FileInfo) bool { return false }
