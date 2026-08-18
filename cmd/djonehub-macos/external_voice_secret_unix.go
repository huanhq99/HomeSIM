//go:build !windows

package main

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func readExternalVoicePasswordFile(path string, maxBytes int64) ([]byte, error) {
	if path == "" || maxBytes <= 0 {
		return nil, errors.New("invalid external voice password file")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "external-voice-password")
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open external voice password file")
	}
	defer file.Close()

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		return nil, errors.New("external voice password file must be a single-link regular file")
	}
	if stat.Uid != uint32(unix.Geteuid()) {
		return nil, errors.New("external voice password file must be owned by the current user")
	}
	if stat.Mode&0o777 != 0o600 {
		return nil, errors.New("external voice password file must have mode 0600")
	}
	if stat.Size < 0 || stat.Size > maxBytes {
		return nil, errors.New("external voice password file is too large")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		for index := range data {
			data[index] = 0
		}
		return nil, errors.New("external voice password file is too large")
	}
	return data, nil
}
