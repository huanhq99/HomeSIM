//go:build !darwin

package remotevoice

import (
	"errors"
	"net"
	"os"
)

func ipcPlatformSupported() error {
	return errors.New("remotevoice: PCM IPC bridge is only supported on macOS")
}

func secureIPCSocketMode(string) error {
	return errors.New("remotevoice: secure socket chmod is unavailable on this platform")
}

func peerEffectiveUID(*net.UnixConn) (uint32, error) {
	return 0, errors.New("remotevoice: getpeereid is unavailable on this platform")
}

func requireCurrentUserOwner(os.FileInfo) error {
	return errors.New("remotevoice: file owner validation is unavailable on this platform")
}
