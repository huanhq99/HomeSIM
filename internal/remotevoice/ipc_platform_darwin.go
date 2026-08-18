//go:build darwin

package remotevoice

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func ipcPlatformSupported() error { return nil }

func secureIPCSocketMode(path string) error {
	return unix.Fchmodat(unix.AT_FDCWD, path, 0o600, unix.AT_SYMLINK_NOFOLLOW)
}

// peerEffectiveUID is the pure-Go equivalent of macOS getpeereid(3): both read
// the kernel's LOCAL_PEERCRED effective UID for the connected AF_UNIX peer.
func peerEffectiveUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("remotevoice: access PCM IPC peer socket: %w", err)
	}
	var (
		uid      uint32
		queryErr error
	)
	if err := raw.Control(func(fd uintptr) {
		credentials, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil {
			queryErr = err
			return
		}
		uid = credentials.Uid
	}); err != nil {
		return 0, fmt.Errorf("remotevoice: inspect PCM IPC peer: %w", err)
	}
	if queryErr != nil {
		return 0, fmt.Errorf("remotevoice: getpeereid PCM IPC peer: %w", queryErr)
	}
	return uid, nil
}

func requireCurrentUserOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("owner metadata unavailable")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return errors.New("owner is not the current effective user")
	}
	return nil
}
