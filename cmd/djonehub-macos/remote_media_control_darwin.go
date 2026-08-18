//go:build darwin

package main

import (
	"fmt"
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func remoteMediaControlPlatformSupported() error { return nil }

func secureRemoteMediaControlSocket(path string) error {
	return unix.Fchmodat(unix.AT_FDCWD, path, 0o600, unix.AT_SYMLINK_NOFOLLOW)
}

func remoteMediaControlPeerEUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
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
		return 0, err
	}
	if queryErr != nil {
		return 0, fmt.Errorf("inspect local media control peer: %w", queryErr)
	}
	return uid, nil
}

func fileOwnedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
